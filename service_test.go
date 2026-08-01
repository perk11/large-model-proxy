package main

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

// TestProcessExitDuringShutdown verifies that when a service process exits during
// shutdown, the proxy completes promptly. This is a regression test for a deadlock
// where monitorProcess's exitWaitGroup.Done() was in a defer that executed after
// serviceMutex.Lock().
//
// The deadlock scenario (without the fix):
// 1. Service process exits BEFORE shutdown signal arrives
// 2. monitorProcess: process.Wait() returns, reads interrupted=false
// 3. monitorProcess: enters else branch, hits test hook, blocks
// 4. Shutdown signal arrives: interrupted=true, signal handler acquires serviceMutex
// 5. signal handler: stopService → waitForProcessToTerminate blocks on exitWaitGroup
// 6. Test releases hook: monitorProcess proceeds to serviceMutex.Lock() → BLOCKS
// 7. exitWaitGroup.Done() is in monitorProcess's defer — never called while blocked
// 8. DEADLOCK: circular wait (Lock() blocked, exitWaitGroup.Wait() blocked)
//
// The fix: call exitWaitGroup.Done() BEFORE any mutex acquisition.
//
// The test uses PROXY_EXIT_HOOK_FILE env var. monitorProcess blocks at this hook
// after reading interrupted=false but before acquiring serviceMutex. The test
// sends SIGINT while monitorProcess is at the hook, then releases the hook.
func TestProcessExitDuringShutdown(t *testing.T) {
	t.Parallel()

	// Hook file: monitorProcess blocks here after reading interrupted=false,
	// waiting for this file to be deleted before acquiring serviceMutex.
	hookDir := t.TempDir()
	hookFile := hookDir + "/exit-hook"
	if err := os.WriteFile(hookFile, []byte{}, 0644); err != nil {
		t.Fatalf("Failed to create hook file: %v", err)
	}

	cfg := Config{
		ResourcesAvailable: map[string]ResourceAvailable{
			"CPU": {Amount: 1},
		},
		ShutDownAfterInactivitySeconds: 120,
		ManagementApi: ManagementApi{
			ListenPort: "2099",
		},
		Services: []ServiceConfig{
			{
				ListenPort:      "2098",
				ProxyTargetHost: "localhost",
				ProxyTargetPort: "12098",
				Command:         "./test-server/test-server",
				Args:            "-p 12098 -exit-after-duration 200ms --ignore-sigterm --sleep-after-writing-pid-duration 100ms",
				ResourceRequirements: map[string]int{
					"CPU": 1,
				},
			},
		},
	}
	StandardizeConfigNamesAndPaths(&cfg, "process-exit-during-shutdown")
	configFilePath := createTempConfig(t, cfg)

	// Start proxy with the hook file env var
	waitChannel := make(chan error, 1)
	cmd, err := startLargeModelProxyWithEnv("process-exit-during-shutdown", configFilePath, "", []string{fmt.Sprintf("PROXY_EXIT_HOOK_FILE=%s", hookFile)}, waitChannel)
	if err != nil {
		t.Fatalf("could not start application: %v", err)
	}

	// Connect to start the service
	conn, err := net.DialTimeout("tcp", "localhost:2098", 5*time.Second)
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("Failed to connect to service: %v", err)
	}
	buf := make([]byte, 64)
	conn.Read(buf)
	t.Logf("Service started")
	conn.Close()

	// Process exits after 200ms (exit-after-duration, --ignore-sigterm).
	// After exit: monitorProcess: process.Wait() returns → reads interrupted=false
	// → enters else branch → hits hook → blocks (waiting for hook file deletion)
	time.Sleep(300 * time.Millisecond)

	// Now send SIGINT. Signal handler:
	// 1. Sets interrupted = true (too late — monitorProcess already read it as false)
	// 2. Acquires serviceMutex
	// 3. stopService: sends SIGTERM (ignored by --ignore-sigterm)
	// 4. stopService: waitForProcessToTerminate blocks on exitWaitGroup
	shutdownStart := time.Now()
	err = cmd.Process.Signal(syscall.SIGINT)
	if err != nil {
		t.Fatalf("Failed to send SIGINT to proxy: %v", err)
	}

	// Wait for signal handler to acquire serviceMutex and enter waitForProcessToTerminate
	time.Sleep(200 * time.Millisecond)

	// Release hook. monitorProcess proceeds to serviceMutex.Lock() → BLOCKS
	// (signal handler holds serviceMutex)
	//
	// WITHOUT the fix: monitorProcess blocks on Lock(). defer never runs.
	// waitForProcessToTerminate hangs for 10s (ProcessCheckTimeout).
	// SIGKILL sent, proxy exits after ~10 seconds.
	//
	// WITH the fix: exitWaitGroup.Done() was called BEFORE the hook.
	// waitForProcessToTerminate returns immediately. No deadlock.
	if err := os.Remove(hookFile); err != nil {
		t.Fatalf("Failed to remove hook file: %v", err)
	}

	select {
	case err = <-waitChannel:
		shutdownDuration := time.Since(shutdownStart)
		t.Logf("Shutdown completed in %v", shutdownDuration)
		if shutdownDuration > 3*time.Second {
			t.Errorf("Shutdown took %v, expected < 3s. This indicates exitWaitGroup.Done() was not called promptly (deadlock occurred)", shutdownDuration)
		}
		if err != nil && err.Error() != "waitid: no child processes" && err.Error() != "wait: no child processes" {
			t.Logf("Proxy exited with: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Errorf("Shutdown took more than 15 seconds — deadlock: exitWaitGroup.Done() was not called before serviceMutex.Lock()")
		_ = cmd.Process.Kill()
	}
}

// TestWaitingConnectionsDecrementedOnServiceStartFailure is a regression test for
// a leak where a connection that triggered a service start was counted as
// "waiting" but the counter was never decremented when the service failed to
// start (process exit, healthcheck failure, startup timeout, etc.). The leaked
// counter left WaitingConnections > 0 forever and broke subsequent status
// checks / idle-shutdown decisions.
//
// It exercises two failure shapes:
//   - a slow failure (healthcheck always fails, then startup timeout) so the
//     waiting state is observable, and
//   - a fast failure (process exits immediately).
//
// Both are repeated several times: if the decrement regressed, the counter
// would accumulate across iterations instead of returning to 0 each time.
func TestWaitingConnectionsDecrementedOnServiceStartFailure(t *testing.T) {
	t.Parallel()

	const managementApiAddress = "localhost:2105"
	const slowProxyAddress = "localhost:2106"
	const fastProxyAddress = "localhost:2107"
	const testName = "waiting-conn-decremented"
	const slowServiceName = testName + "_slow"
	const fastServiceName = testName + "_fast"

	slowStartupTimeoutMs := uint(700)
	cfg := Config{
		ResourcesAvailable: map[string]ResourceAvailable{"CPU": {Amount: 1}},
		ManagementApi:      ManagementApi{ListenPort: "2105"},
		Services: []ServiceConfig{
			{
				Name:                            "slow",
				ListenPort:                      "2106",
				ProxyTargetHost:                 "localhost",
				ProxyTargetPort:                 "12106",
				Command:                         "./test-server/test-server",
				Args:                            "-p 12106 -startup-duration 24h",
				HealthcheckCommand:              "false",
				HealthcheckIntervalMilliseconds: 100,
				StartupTimeoutMilliseconds:      &slowStartupTimeoutMs,
				ResourceRequirements:            map[string]int{"CPU": 1},
			},
			{
				Name:                 "fast",
				ListenPort:           "2107",
				ProxyTargetHost:      "localhost",
				ProxyTargetPort:      "12107",
				Command:              "exit",
				Args:                 "1",
				ResourceRequirements: map[string]int{"CPU": 1},
			},
		},
	}
	StandardizeConfigNamesAndPaths(&cfg, testName)
	configFilePath := createTempConfig(t, cfg)

	waitChannel := make(chan error, 1)
	cmd, err := startLargeModelProxy("waiting-conn-decremented", configFilePath, "", waitChannel)
	if err != nil {
		t.Fatalf("could not start application: %v", err)
	}
	defer func() {
		if err := stopApplication(cmd, waitChannel); err != nil {
			t.Errorf("failed to stop application: %v", err)
		}
		for _, address := range []string{slowProxyAddress, fastProxyAddress, managementApiAddress} {
			if err := checkPortClosed(address); err != nil {
				t.Errorf("port %s is still open after application exit: %v", address, err)
			}
		}
	}()

	statusResponse := getStatusFromManagementAPI(t, managementApiAddress)
	verifyServiceStatus(t, statusResponse, slowServiceName, ServiceStateStopped, 0, 0, map[string]int{"CPU": 0})
	verifyServiceStatus(t, statusResponse, fastServiceName, ServiceStateStopped, 0, 0, map[string]int{"CPU": 0})

	// Slow failure path: the connection waits while the healthcheck runs, so we
	// can observe WaitingConnections rise to 1, then fall back to 0 once the
	// service fails to start and the proxy closes the client connection.
	for iteration := 0; iteration < 3; iteration++ {
		con, err := net.DialTimeout("tcp", slowProxyAddress, 3*time.Second)
		if err != nil {
			t.Fatalf("slow iteration %d: failed to connect to proxy: %v", iteration, err)
		}
		waitForWaitingConnections(t, managementApiAddress, slowServiceName, 1, 1*time.Second)
		assertRemoteClosedWithin(t, con, 2*time.Second)
		_ = con.Close()

		statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
		verifyServiceStatus(t, statusResponse, slowServiceName, ServiceStateStopped, 0, 0, map[string]int{"CPU": 0})
	}

	// Fast failure path: the process exits immediately. There is no observable
	// waiting window, but the counter must still return to 0 after each failure.
	for iteration := 0; iteration < 3; iteration++ {
		con, err := net.DialTimeout("tcp", fastProxyAddress, 3*time.Second)
		if err != nil {
			t.Fatalf("fast iteration %d: failed to connect to proxy: %v", iteration, err)
		}
		assertRemoteClosedWithin(t, con, 2*time.Second)
		_ = con.Close()

		statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
		verifyServiceStatus(t, statusResponse, fastServiceName, ServiceStateStopped, 0, 0, map[string]int{"CPU": 0})
	}
}

// TestWaitingConnectionCountReleasedWhenClientDisconnects is a regression test for
// a counting bug where a connection that disconnects while it is still in the
// "waiting" state (waiting for resources so its service can start) keeps being
// counted as a waiting connection until the resource wait times out. The stale
// counter inflated WaitingConnections and, because the waiting count never
// reached zero, could delay or block idle shutdown / eviction decisions for up
// to MaxTimeToWaitForServiceToCloseConnectionBeforeGivingUpSeconds.
//
// Scenario:
//  1. A "holder" service starts and keeps a proxied connection (and therefore
//     its resource) open for a long time.
//  2. A second connection targets another service that needs the same resource.
//     It cannot start and is counted as a waiting connection (WaitingConnections == 1).
//  3. The waiting client disconnects.
//  4. WaitingConnections must drop back to 0 promptly, without waiting for the
//     holder to release the resource.
func TestWaitingConnectionCountReleasedWhenClientDisconnects(t *testing.T) {
	t.Parallel()

	const managementApiAddress = "localhost:2110"
	const holderProxyAddress = "localhost:2111"
	const waiterProxyAddress = "localhost:2112"
	const testName = "waiting-conn-released-on-disconnect"
	const holderServiceName = testName + "_holder"
	const waiterServiceName = testName + "_waiter"

	// The holder keeps its proxied connection open well beyond the duration of
	// this test so the resource stays unavailable to the waiter the whole time.
	cfg := Config{
		ResourcesAvailable: map[string]ResourceAvailable{"TestResource": {Amount: 1}},
		ManagementApi:      ManagementApi{ListenPort: "2110"},
		Services: []ServiceConfig{
			{
				Name:                 "holder",
				ListenPort:           "2111",
				ProxyTargetHost:      "localhost",
				ProxyTargetPort:      "12111",
				Command:              "./test-server/test-server",
				Args:                 "-p 12111 -sleep-after-writing-pid-duration 30s",
				ResourceRequirements: map[string]int{"TestResource": 1},
			},
			{
				Name:                 "waiter",
				ListenPort:           "2112",
				ProxyTargetHost:      "localhost",
				ProxyTargetPort:      "12112",
				Command:              "./test-server/test-server",
				Args:                 "-p 12112",
				ResourceRequirements: map[string]int{"TestResource": 1},
			},
		},
	}
	StandardizeConfigNamesAndPaths(&cfg, testName)
	configFilePath := createTempConfig(t, cfg)

	waitChannel := make(chan error, 1)
	cmd, err := startLargeModelProxy("waiting-conn-released-on-disconnect", configFilePath, "", waitChannel)
	if err != nil {
		t.Fatalf("could not start application: %v", err)
	}
	defer func() {
		if err := stopApplication(cmd, waitChannel); err != nil {
			t.Errorf("failed to stop application: %v", err)
		}
		for _, address := range []string{holderProxyAddress, waiterProxyAddress, managementApiAddress} {
			if err := checkPortClosed(address); err != nil {
				t.Errorf("port %s is still open after application exit: %v", address, err)
			}
		}
	}()

	// 1. Start the holder and keep its connection open so it reserves TestResource.
	holderConn, err := net.DialTimeout("tcp", holderProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to holder: %v", err)
	}
	defer func() { _ = holderConn.Close() }()
	readPidFromOpenConnection(t, holderConn)
	// holder is now running with one proxied connection, holding TestResource.
	statusResponse := getStatusFromManagementAPI(t, managementApiAddress)
	verifyServiceStatus(t, statusResponse, holderServiceName, ServiceStateRunning, 0, 1, map[string]int{"TestResource": 1})

	// 2. A new connection to the waiter must wait for TestResource.
	waiterConn, err := net.DialTimeout("tcp", waiterProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to waiter: %v", err)
	}
	waitForWaitingConnections(t, managementApiAddress, waiterServiceName, 1, 3*time.Second)

	// 3. The waiting client disconnects.
	_ = waiterConn.Close()

	// 4. WaitingConnections must return to 0 promptly, well before the holder
	//    releases the resource (which it won't do for 30s).
	waitForWaitingConnections(t, managementApiAddress, waiterServiceName, 0, 3*time.Second)
}

// TestResourcesReleasedWhenProcessExitsDuringConnectAndConsiderStoppedFalse is a
// regression test for a resource / runningServices leak. When a service process
// exits during the connect-retry phase, startService's process-exit branch used
// to only release the reserved counter and rely on monitorProcess to finish the
// cleanup. But monitorProcess passes *ConsiderStoppedOnProcessExit as the cleanup
// flag, so when that option is false monitorProcess skips cleanup entirely and
// resourcesInUse is never decremented nor is the runningServices entry removed
// (the service is then reported as "starting" forever). This test opts into
// ConsiderStoppedOnProcessExit=false, drives the process to exit before it ever
// listens, and asserts that the resource is fully freed (in_use == 0, free ==
// total) and the service returns to "stopped".
func TestResourcesReleasedWhenProcessExitsDuringConnectAndConsiderStoppedFalse(t *testing.T) {
	t.Parallel()

	const managementApiAddress = "localhost:2120"
	const serviceProxyAddress = "localhost:2121"
	const testName = "process-exit-during-connect-no-monitor"
	const serviceName = testName + "_dying-process"

	// The service process sleeps 30s before it would start listening on the
	// target port, but exits after 500ms — so the proxy never manages to
	// connect and the process exits during the connect-retry phase, well before
	// the 60s startup timeout.
	startupTimeoutMs := uint(60000)
	considerStoppedOnProcessExit := false
	cfg := Config{
		ResourcesAvailable: map[string]ResourceAvailable{"TestResource": {Amount: 1}},
		ManagementApi:      ManagementApi{ListenPort: "2120"},
		Services: []ServiceConfig{
			{
				Name:                         "dying-process",
				ListenPort:                   "2121",
				ProxyTargetHost:              "localhost",
				ProxyTargetPort:              "12121",
				Command:                      "./test-server/test-server",
				Args:                         "-p 12121 -sleep-before-listening 30s -exit-after-duration 500ms",
				StartupTimeoutMilliseconds:   &startupTimeoutMs,
				ConsiderStoppedOnProcessExit: &considerStoppedOnProcessExit,
				RestartOnConnectionFailure:   false,
				ResourceRequirements:         map[string]int{"TestResource": 1},
			},
		},
	}
	StandardizeConfigNamesAndPaths(&cfg, testName)
	configFilePath := createTempConfig(t, cfg)

	waitChannel := make(chan error, 1)
	cmd, err := startLargeModelProxy(testName, configFilePath, "", waitChannel)
	if err != nil {
		t.Fatalf("could not start application: %v", err)
	}
	defer func() {
		if err := stopApplication(cmd, waitChannel); err != nil {
			t.Errorf("failed to stop application: %v", err)
		}
		for _, address := range []string{serviceProxyAddress, managementApiAddress} {
			if err := checkPortClosed(address); err != nil {
				t.Errorf("port %s is still open after application exit: %v", address, err)
			}
		}
	}()

	// Connect a client to trigger startService. The client stays connected while
	// the service sits in the connect-retry phase.
	clientConn, err := net.DialTimeout("tcp", serviceProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to service proxy: %v", err)
	}
	defer func() { _ = clientConn.Close() }()

	// The service reserves TestResource and enters the starting/connect-retry
	// phase, so the resource is held: in_use == 1 and free == 0. Poll, since the
	// reservation and the state transition are observed asynchronously.
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp := getStatusFromManagementAPI(t, managementApiAddress)
		if info, ok := resp.Resources["TestResource"]; ok && info.InUse == 1 {
			verifyResourceUsage(t, resp,
				map[string]int{"TestResource": 1}, // reserved by starting services
				map[string]int{"TestResource": 0}, // free (held by the starting service)
				map[string]int{"TestResource": 1}, // in_use
				map[string]int{"TestResource": 1}, // total
			)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("resource TestResource was never held (in_use never reached 1) within %s", 3*time.Second)
		}
		time.Sleep(10 * time.Millisecond)
	}

	resp := waitForServiceState(t, managementApiAddress, serviceName, ServiceStateStopped, 5*time.Second)
	verifyResourceUsage(t, resp,
		map[string]int{"TestResource": 0}, // reserved by starting services
		map[string]int{"TestResource": 1}, // free (full total — no leak)
		map[string]int{"TestResource": 0}, // in_use (no leak)
		map[string]int{"TestResource": 1}, // total
	)
}

// TestWaitingConnectionCountReleasedWhenClientDisconnectsDuringStartup verifies
// that a client which disconnects while its service is still in the startup
// phase (after resources were reserved but before the service is ready) also
// releases its waiting-connection count promptly. This covers the healthcheck
// and connect-retry phases, not just the resource-wait phase covered by
// TestWaitingConnectionCountReleasedWhenClientDisconnects. Without aborting
// startup on client disconnect, the counter would stay inflated until the
// service eventually started or hit its startup timeout (potentially minutes).
func TestWaitingConnectionCountReleasedWhenClientDisconnectsDuringStartup(t *testing.T) {
	t.Parallel()

	const managementApiAddress = "localhost:2113"
	const slowConnectProxyAddress = "localhost:2114"
	const slowHealthcheckProxyAddress = "localhost:2115"
	const testName = "waiting-conn-released-startup"
	const slowConnectServiceName = testName + "_slow-connect"
	const slowHealthcheckServiceName = testName + "_slow-healthcheck"

	startupTimeoutMs := uint(60000)
	cfg := Config{
		ResourcesAvailable: map[string]ResourceAvailable{"CPU": {Amount: 2}},
		ManagementApi:      ManagementApi{ListenPort: "2113"},
		Services: []ServiceConfig{
			{
				// No healthcheck command, so the service sits in the connect-retry
				// phase (tryConnectingUntilTimeoutOrProcessExit) for a long time.
				Name:                       "slow-connect",
				ListenPort:                 "2114",
				ProxyTargetHost:            "localhost",
				ProxyTargetPort:            "12114",
				Command:                    "./test-server/test-server",
				Args:                       "-p 12114 -sleep-before-listening 30s",
				StartupTimeoutMilliseconds: &startupTimeoutMs,
				ResourceRequirements:       map[string]int{"CPU": 1},
			},
			{
				// A healthcheck command that always fails, so the service sits in
				// the healthcheck phase (performHealthCheck) for a long time.
				Name:                            "slow-healthcheck",
				ListenPort:                      "2115",
				ProxyTargetHost:                 "localhost",
				ProxyTargetPort:                 "12115",
				Command:                         "./test-server/test-server",
				Args:                            "-p 12115 -sleep-before-listening 30s",
				HealthcheckCommand:              "false",
				HealthcheckIntervalMilliseconds: 200,
				StartupTimeoutMilliseconds:      &startupTimeoutMs,
				ResourceRequirements:            map[string]int{"CPU": 1},
			},
		},
	}
	StandardizeConfigNamesAndPaths(&cfg, testName)
	configFilePath := createTempConfig(t, cfg)

	waitChannel := make(chan error, 1)
	cmd, err := startLargeModelProxy(testName, configFilePath, "", waitChannel)
	if err != nil {
		t.Fatalf("could not start application: %v", err)
	}
	defer func() {
		if err := stopApplication(cmd, waitChannel); err != nil {
			t.Errorf("failed to stop application: %v", err)
		}
		for _, address := range []string{slowConnectProxyAddress, slowHealthcheckProxyAddress, managementApiAddress} {
			if err := checkPortClosed(address); err != nil {
				t.Errorf("port %s is still open after application exit: %v", address, err)
			}
		}
	}()

	// slow-connect: resources are reserved immediately, then the service spends
	// 30s before listening, so the connection waits in the connect-retry phase.
	connA, err := net.DialTimeout("tcp", slowConnectProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to slow-connect: %v", err)
	}
	waitForWaitingConnections(t, managementApiAddress, slowConnectServiceName, 1, 3*time.Second)

	// slow-healthcheck: the healthcheck command always fails, so the connection
	// waits in the healthcheck phase until the startup timeout.
	connB, err := net.DialTimeout("tcp", slowHealthcheckProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to slow-healthcheck: %v", err)
	}
	waitForWaitingConnections(t, managementApiAddress, slowHealthcheckServiceName, 1, 3*time.Second)

	// Both waiting clients disconnect while their services are still starting.
	_ = connA.Close()
	_ = connB.Close()

	// The waiting counts must return to 0 promptly, well before the 30s/60s
	// startup windows would otherwise elapse.
	waitForWaitingConnections(t, managementApiAddress, slowConnectServiceName, 0, 5*time.Second)
	waitForWaitingConnections(t, managementApiAddress, slowHealthcheckServiceName, 0, 5*time.Second)
}

// TestQueuedClientDisconnectDropsWaitingCount is a regression test for prompt queued-client disconnect
// covering clients that arrive while their target service is ALREADY starting.
// Such a client cannot TryLock the service's manageMutex and so it QUEUES behind
// the holder. Previously the queue was a plain manageMutex.Lock() that never
// selected on this queued client's clientDisconnected signal, so:
//   - its waiting-connection count stayed inflated until the holder finished or
//     aborted (violating the prompt-drop requirement), and
//   - when the holder aborted, the queued client recursed into a fresh
//     startService even though it had already disconnected (wasted work).
//
// Scenario (a single slow-starting service; both clients target its proxy port):
//  1. Client A dials → service starts and gets stuck in the connect-retry phase,
//     holding manageMutex (state == starting, waiting == 1).
//  2. Client B dials the SAME port → B queues on manageMutex (waiting == 2).
//  3. Client B disconnects while queued → waiting must drop back to 1 PROMPTLY
//     (the H1 assertion: under the old code it stayed at 2 until A's ~30s startup
//     finished).
//  4. Client A disconnects → waiting drops to 0 promptly.
func TestQueuedClientDisconnectDropsWaitingCount(t *testing.T) {
	t.Parallel()

	const managementApiAddress = "localhost:2122"
	const serviceProxyAddress = "localhost:2123"
	const testName = "queued-client-disconnect"
	const serviceName = testName + "_svc"

	// One slow-starting service: a static resource (immediately available, so the
	// service starts right away) and a 30s pre-listen sleep with no healthcheck,
	// which keeps it parked in the connect-retry phase — and thus holding
	// manageMutex — for the whole test.
	startupTimeoutMs := uint(60000)
	cfg := Config{
		ResourcesAvailable: map[string]ResourceAvailable{"CPU": {Amount: 2}},
		ManagementApi:      ManagementApi{ListenPort: "2122"},
		Services: []ServiceConfig{
			{
				Name:                       "svc",
				ListenPort:                 "2123",
				ProxyTargetHost:            "localhost",
				ProxyTargetPort:            "12123",
				Command:                    "./test-server/test-server",
				Args:                       "-p 12123 -sleep-before-listening 30s",
				StartupTimeoutMilliseconds: &startupTimeoutMs,
				ResourceRequirements:       map[string]int{"CPU": 1},
			},
		},
	}
	StandardizeConfigNamesAndPaths(&cfg, testName)
	configFilePath := createTempConfig(t, cfg)

	waitChannel := make(chan error, 1)
	cmd, err := startLargeModelProxy(testName, configFilePath, "", waitChannel)
	if err != nil {
		t.Fatalf("could not start application: %v", err)
	}
	defer func() {
		if err := stopApplication(cmd, waitChannel); err != nil {
			t.Errorf("failed to stop application: %v", err)
		}
		if err := checkPortClosed(serviceProxyAddress); err != nil {
			t.Errorf("port %s is still open after application exit: %v", serviceProxyAddress, err)
		}
		if err := checkPortClosed(managementApiAddress); err != nil {
			t.Errorf("port %s is still open after application exit: %v", managementApiAddress, err)
		}
	}()

	// 1. Client A dials the service proxy port → service starts and gets stuck in
	//    the connect-retry phase, holding manageMutex. Wait until the service is
	//    registered and reports "starting" so that B is guaranteed to take the
	//    queued path rather than racing the registration.
	clientA, err := net.DialTimeout("tcp", serviceProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect client A: %v", err)
	}
	defer func() { _ = clientA.Close() }()
	waitForServiceState(t, managementApiAddress, serviceName, ServiceStateStarting, 5*time.Second)
	waitForWaitingConnections(t, managementApiAddress, serviceName, 1, 3*time.Second)

	// 2. Client B dials the SAME service proxy port → B cannot TryLock
	//    manageMutex (held by A) and queues. waiting_connections must be 2.
	clientB, err := net.DialTimeout("tcp", serviceProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect client B: %v", err)
	}
	defer func() { _ = clientB.Close() }()
	waitForWaitingConnections(t, managementApiAddress, serviceName, 2, 3*time.Second)

	// 3. Client B disconnects WHILE QUEUED. Its waiting count must drop back to 1
	//    promptly — well before A's ~30s startup finishes. Under the old code
	//    (blocking manageMutex.Lock() ignoring clientDisconnected) this stayed at
	//    2 until A's startup completed/aborted, so the short timeout fails fast on
	//    a regression.
	_ = clientB.Close()
	waitForWaitingConnections(t, managementApiAddress, serviceName, 1, 3*time.Second)

	// 4. Client A (the holder) disconnects. Startup is aborted via the holder
	//    disconnect path and waiting drops to 0 promptly.
	_ = clientA.Close()
	waitForWaitingConnections(t, managementApiAddress, serviceName, 0, 5*time.Second)
	waitForServiceState(t, managementApiAddress, serviceName, ServiceStateStopped, 10*time.Second)
}

// Scenario: the healthcheck command always fails ("false"), so the service
// sits in the healthcheck phase. The process itself exits after 500ms (well
// before the 60s startup timeout), so the process dies DURING the healthcheck
// phase. With ConsiderStoppedOnProcessExit=true, performHealthCheck must abort
// the moment the process exits instead of keep re-spawning the failing
// healthcheck subprocess until StartupTimeout. The sibling test
// TestProcessExitDuringHealthCheckDoesNotAbortWhenConsiderStoppedOnProcessExitFalse
// verifies the opposite behavior holds when ConsiderStoppedOnProcessExit is
// false.
func TestProcessExitDuringHealthCheckAbortsHealthcheckLoop(t *testing.T) {
	t.Parallel()

	const managementApiAddress = "localhost:2124"
	const serviceProxyAddress = "localhost:2125"
	const testName = "process-exit-during-healthcheck"
	const serviceName = testName + "_dying-process"

	// The healthcheck command always fails, so the service sits in the
	// healthcheck phase. The process sleeps 30s before it would start listening
	// on the target port, but exits after 500ms — so the process dies during the
	// healthcheck phase, well before the 60s startup timeout.
	startupTimeoutMs := uint(60000)
	considerStoppedOnProcessExit := true
	cfg := Config{
		ResourcesAvailable: map[string]ResourceAvailable{"TestResource": {Amount: 1}},
		ManagementApi:      ManagementApi{ListenPort: "2124"},
		Services: []ServiceConfig{
			{
				Name:                            "dying-process",
				ListenPort:                      "2125",
				ProxyTargetHost:                 "localhost",
				ProxyTargetPort:                 "12125",
				Command:                         "./test-server/test-server",
				Args:                            "-p 12125 -sleep-before-listening 30s -exit-after-duration 500ms",
				HealthcheckCommand:              "false",
				HealthcheckIntervalMilliseconds: 200,
				StartupTimeoutMilliseconds:      &startupTimeoutMs,
				ConsiderStoppedOnProcessExit:    &considerStoppedOnProcessExit,
				RestartOnConnectionFailure:      false,
				ResourceRequirements:            map[string]int{"TestResource": 1},
			},
		},
	}
	StandardizeConfigNamesAndPaths(&cfg, testName)
	configFilePath := createTempConfig(t, cfg)

	waitChannel := make(chan error, 1)
	cmd, err := startLargeModelProxy(testName, configFilePath, "", waitChannel)
	if err != nil {
		t.Fatalf("could not start application: %v", err)
	}
	defer func() {
		if err := stopApplication(cmd, waitChannel); err != nil {
			t.Errorf("failed to stop application: %v", err)
		}
		for _, address := range []string{serviceProxyAddress, managementApiAddress} {
			if err := checkPortClosed(address); err != nil {
				t.Errorf("port %s is still open after application exit: %v", address, err)
			}
		}
	}()

	// Connect a client to trigger startService. The client stays connected while
	// the service sits in the healthcheck phase.
	clientConn, err := net.DialTimeout("tcp", serviceProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to service proxy: %v", err)
	}
	defer func() { _ = clientConn.Close() }()

	// The service reserves TestResource and enters the starting/healthcheck
	// phase, so the resource is held: in_use == 1 and free == 0. Poll, since the
	// reservation and the state transition are observed asynchronously.
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp := getStatusFromManagementAPI(t, managementApiAddress)
		if info, ok := resp.Resources["TestResource"]; ok && info.InUse == 1 {
			verifyResourceUsage(t, resp,
				map[string]int{"TestResource": 1}, // reserved by starting services
				map[string]int{"TestResource": 0}, // free (held by the starting service)
				map[string]int{"TestResource": 1}, // in_use
				map[string]int{"TestResource": 1}, // total
			)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("resource TestResource was never held (in_use never reached 1) within %s", 3*time.Second)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// With the fix, performHealthCheck aborts as soon as the process exits
	// (~500ms) instead of spinning for the full 60s StartupTimeout. The service
	// returns to "stopped" and the resource is fully freed promptly — well
	// before the 60s window. waitForServiceState's 5s deadline proves the loop
	// did not run to StartupTimeout.
	resp := waitForServiceState(t, managementApiAddress, serviceName, ServiceStateStopped, 5*time.Second)
	verifyResourceUsage(t, resp,
		map[string]int{"TestResource": 0}, // reserved by starting services
		map[string]int{"TestResource": 1}, // free (full total — no leak)
		map[string]int{"TestResource": 0}, // in_use (no leak)
		map[string]int{"TestResource": 1}, // total
	)

	// The waiting client connection must be closed (EOF) promptly too, instead
	// of hanging until the startup timeout.
	assertRemoteClosedWithin(t, clientConn, 2*time.Second)
}

// TestProcessExitDuringHealthCheckDoesNotAbortWhenConsiderStoppedOnProcessExitFalse
// is the counterpart to TestProcessExitDuringHealthCheckAbortsHealthcheckLoop.
// For services whose process detaches from the proxy (ConsiderStoppedOnProcessExit=false,
// e.g. docker containers), the child process exiting is expected and does not
// mean the service is down, so performHealthCheck must NOT abort on process
// exit — it keeps re-running the healthcheck command until StartupTimeout.
//
// Scenario: same as the abort test (healthcheck command always fails, process
// exits after 500ms), but ConsiderStoppedOnProcessExit=false and StartupTimeout
// is only 3s. After the process exits we assert the resource is STILL held (the
// loop did not abort), and that it is only released once the StartupTimeout
// elapses and the healthcheck times out.
func TestProcessExitDuringHealthCheckDoesNotAbortWhenConsiderStoppedOnProcessExitFalse(t *testing.T) {
	t.Parallel()

	const managementApiAddress = "localhost:2126"
	const serviceProxyAddress = "localhost:2127"
	const testName = "process-exit-during-healthcheck-no-abort"
	const serviceName = testName + "_dying-process"
	const proxyTargetPort = "12126"

	// The process exits after 500ms, but the healthcheck loop must keep going
	// until the 3s StartupTimeout, since ConsiderStoppedOnProcessExit is false.
	startupTimeoutMs := uint(3000)
	considerStoppedOnProcessExit := false
	cfg := Config{
		ResourcesAvailable: map[string]ResourceAvailable{"TestResource": {Amount: 1}},
		ManagementApi:      ManagementApi{ListenPort: "2126"},
		Services: []ServiceConfig{
			{
				Name:                            "dying-process",
				ListenPort:                      "2127",
				ProxyTargetHost:                 "localhost",
				ProxyTargetPort:                 proxyTargetPort,
				Command:                         "./test-server/test-server",
				Args:                            "-p " + proxyTargetPort + " -sleep-before-listening 30s -exit-after-duration 500ms",
				HealthcheckCommand:              "false",
				HealthcheckIntervalMilliseconds: 200,
				StartupTimeoutMilliseconds:      &startupTimeoutMs,
				ConsiderStoppedOnProcessExit:    &considerStoppedOnProcessExit,
				RestartOnConnectionFailure:      false,
				ResourceRequirements:            map[string]int{"TestResource": 1},
			},
		},
	}
	StandardizeConfigNamesAndPaths(&cfg, testName)
	configFilePath := createTempConfig(t, cfg)

	waitChannel := make(chan error, 1)
	cmd, err := startLargeModelProxy(testName, configFilePath, "", waitChannel)
	if err != nil {
		t.Fatalf("could not start application: %v", err)
	}
	defer func() {
		if err := stopApplication(cmd, waitChannel); err != nil {
			t.Errorf("failed to stop application: %v", err)
		}
		for _, address := range []string{serviceProxyAddress, managementApiAddress} {
			if err := checkPortClosed(address); err != nil {
				t.Errorf("port %s is still open after application exit: %v", address, err)
			}
		}
	}()

	// Connect a client to trigger startService. The client stays connected while
	// the service sits in the healthcheck phase.
	clientConn, err := net.DialTimeout("tcp", serviceProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to service proxy: %v", err)
	}
	defer func() { _ = clientConn.Close() }()

	// The service reserves TestResource and enters the starting/healthcheck
	// phase, so the resource is held: in_use == 1 and free == 0. Poll, since the
	// reservation and the state transition are observed asynchronously.
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp := getStatusFromManagementAPI(t, managementApiAddress)
		if info, ok := resp.Resources["TestResource"]; ok && info.InUse == 1 {
			verifyResourceUsage(t, resp,
				map[string]int{"TestResource": 1}, // reserved by starting services
				map[string]int{"TestResource": 0}, // free (held by the starting service)
				map[string]int{"TestResource": 1}, // in_use
				map[string]int{"TestResource": 1}, // total
			)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("resource TestResource was never held (in_use never reached 1) within %s", 3*time.Second)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The process exits after 500ms. Because ConsiderStoppedOnProcessExit is
	// false, monitorProcess does not clean up and performHealthCheck must NOT
	// abort. So even after the process has exited the service stays "starting"
	// and the resource stays held until the StartupTimeout (3s) elapses. Waiting
	// 1s here lands safely past the 500ms process exit but ~2s before the 3s
	// StartupTimeout, proving the loop kept going.
	time.Sleep(1 * time.Second)
	resp := getStatusFromManagementAPI(t, managementApiAddress)
	// The client is still waiting for the service to become ready (the healthcheck
	// is still failing), so it counts as one waiting connection.
	verifyServiceStatus(t, resp, serviceName, ServiceStateStarting, 1, 0, map[string]int{"TestResource": 1})
	verifyResourceUsage(t, resp,
		map[string]int{"TestResource": 1}, // reserved by starting services
		map[string]int{"TestResource": 0}, // free (still held by the starting service)
		map[string]int{"TestResource": 1}, // in_use
		map[string]int{"TestResource": 1}, // total
	)

	// Once the 3s StartupTimeout elapses, the healthcheck times out, startService
	// returns, and the service stops and frees the resource. The 5s deadline
	// proves cleanup did happen (as opposed to hanging forever).
	resp = waitForServiceState(t, managementApiAddress, serviceName, ServiceStateStopped, 5*time.Second)
	verifyResourceUsage(t, resp,
		map[string]int{"TestResource": 0}, // reserved by starting services
		map[string]int{"TestResource": 1}, // free (full total — no leak)
		map[string]int{"TestResource": 0}, // in_use (no leak)
		map[string]int{"TestResource": 1}, // total
	)

	// The waiting client connection is closed (EOF) once the healthcheck times
	// out and the service stops.
	assertRemoteClosedWithin(t, clientConn, 2*time.Second)
}
