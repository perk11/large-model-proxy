package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
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

// TestNoWastedCheckCommandWhenStaticResourceIsBottleneck pins the two-pass fix
// in findFirstMissingResourceWhenServiceMutexIsLocked (Issue I2).
//
// When a service requires BOTH a CheckCommand-backed resource and a
// statically-tracked resource, and the STATIC resource is the bottleneck, the
// proxy must NOT register a CheckCommand first-change channel / Unpause the
// monitor for the CheckCommand resource. Doing so forces one unnecessary
// external CheckCommand run for a service that cannot start anyway because of
// the static shortage.
//
// Setup: a holder service holds the single unit of a static resource and has an
// active proxied connection (so canBeStopped returns false and it cannot be
// evicted). A victim service requires one unit of the static resource AND one
// unit of a CheckCommand resource whose CheckCommand increments a counter file.
// The resource monitor runs that CheckCommand once at startup (counter 0 -> 1);
// afterwards, with no listener registered for the CheckCommand resource, the
// monitor never re-runs it (its timer is only re-armed when a listener exists or
// it is Unpause-poked). We drive several victim connection attempts — each one
// forces one pass through findFirstMissingResourceWhenServiceMutexIsLocked with
// firstCheckNeeded=true. Under the OLD single-pass loop, a pass whose random map
// iteration visited the CheckCommand resource BEFORE the static one registered a
// first-change channel and Unpause-poked the monitor (one wasted CheckCommand
// run -> counter increments) before returning the static bottleneck. Under the
// fixed two-pass code the static bottleneck is returned WITHOUT ever touching
// the CheckCommand resource, so the counter never advances past its startup
// value of 1.
func TestNoWastedCheckCommandWhenStaticResourceIsBottleneck(t *testing.T) {
	t.Parallel()

	const managementApiAddress = "localhost:2160"
	const holderProxyAddress = "localhost:2161"
	const victimProxyAddress = "localhost:2162"
	const testName = "no-wasted-check-static-bottleneck"
	const holderServiceName = testName + "_holder"
	const victimServiceName = testName + "_victim"
	const staticResource = "StaticSlot"
	const checkedResource = "Checked"
	const counterFile = "test-logs/no-wasted-check.counter.txt"

	// Ensure test-logs/ exists (standalone parallel tests may run before any
	// proxy start creates it), then initialize the counter file at 0; the
	// monitor's startup CheckCommand bumps it to 1 once the proxy starts.
	if err := os.MkdirAll("test-logs", 0755); err != nil {
		t.Fatalf("could not create test-logs directory: %v", err)
	}
	if err := os.WriteFile(counterFile, []byte("0"), 0644); err != nil {
		t.Fatalf("could not write counter file: %v", err)
	}

	cfg := Config{
		ResourcesAvailable: map[string]ResourceAvailable{
			staticResource: {Amount: 1}, // no CheckCommand -> statically tracked
			checkedResource: {
				CheckCommand:                           "read -r n < " + counterFile + "; n=$((n+1)); printf '%d\\n' \"$n\" | tee " + counterFile,
				CheckWhenNotEnoughIntervalMilliseconds: 1000,
			},
		},
		LogLevel:      LogLevelDebug,
		ManagementApi: ManagementApi{ListenPort: "2160"},
		Services: []ServiceConfig{
			{
				Name:            "holder",
				ListenPort:      "2161",
				ProxyTargetHost: "localhost",
				ProxyTargetPort: "12161",
				Command:         "./test-server/test-server",
				// sleep-after-writing-pid-duration keeps the proxied connection
				// open for the whole test, so canBeStopped returns false (proxied
				// >= 1) and the holder cannot be evicted to free StaticSlot for the
				// victim — the static resource stays the bottleneck.
				Args:                 "-p 12161 -healthcheck-port 2163 -sleep-after-writing-pid-duration 60s",
				ResourceRequirements: map[string]int{staticResource: 1},
			},
			{
				Name:                 "victim",
				ListenPort:           "2162",
				ProxyTargetHost:      "localhost",
				ProxyTargetPort:      "12162",
				Command:              "./test-server/test-server",
				Args:                 "-p 12162 -healthcheck-port 2164",
				ResourceRequirements: map[string]int{staticResource: 1, checkedResource: 1},
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
		for _, address := range []string{holderProxyAddress, victimProxyAddress, managementApiAddress} {
			if err := checkPortClosed(address); err != nil {
				t.Errorf("port %s is still open after application exit: %v", address, err)
			}
		}
	}()

	// Wait for the resource monitor's startup CheckCommand to run (counter 0 -> 1).
	deadline := time.Now().Add(3 * time.Second)
	for {
		if readCounterValue(t, counterFile) >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("startup CheckCommand never ran; counter stuck at 0")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Connect a client to the holder so it reserves the single static slot,
	// reaches "running", and — crucially — keeps a proxied connection open so
	// canBeStopped returns false and the holder cannot be evicted for the victim.
	holderConn, err := net.DialTimeout("tcp", holderProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to holder proxy: %v", err)
	}
	defer func() { _ = holderConn.Close() }()

	// Poll until the holder is running, holds the static slot, and reports a
	// proxied connection (which makes it non-evictable).
	deadline = time.Now().Add(5 * time.Second)
	for {
		resp := getStatusFromManagementAPI(t, managementApiAddress)
		holderRunning := false
		for _, svc := range resp.Services {
			if svc.Name == holderServiceName && svc.Status == ServiceStateRunning && svc.ProxiedConnections >= 1 {
				holderRunning = true
			}
		}
		if holderRunning && resp.Resources[staticResource].InUse == 1 {
			verifyResourceUsage(t, resp,
				map[string]int{staticResource: 0}, // reserved by starting services
				map[string]int{staticResource: 0}, // free: the holder holds the only unit
				map[string]int{staticResource: 1}, // in_use
				map[string]int{staticResource: 1}, // total
			)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("holder did not reach running+proxied holding StaticSlot within 5s")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Baseline: with no listener registered for the CheckCommand resource, the
	// monitor must NOT run it again on its own. Give it a brief window to surface
	// any spurious run, then snapshot the baseline (expected to be 1).
	time.Sleep(300 * time.Millisecond)
	baseline := readCounterValue(t, counterFile)
	if baseline != 1 {
		t.Fatalf("counter expected to be 1 after startup check, got %d", baseline)
	}

	// Drive several victim connection attempts. Each attempt forces one pass
	// through findFirstMissingResourceWhenServiceMutexIsLocked with
	// firstCheckNeeded=true. Under the old single-pass loop, ~half of these (map
	// iteration order) registered a first-change channel and Unpause-poked the
	// CheckCommand monitor before hitting the static bottleneck; under the fixed
	// two-pass code the static bottleneck is returned without touching the
	// CheckCommand resource at all.
	for i := 0; i < 6; i++ {
		victimConn, err := net.DialTimeout("tcp", victimProxyAddress, 3*time.Second)
		if err != nil {
			t.Fatalf("victim attempt %d: failed to connect: %v", i, err)
		}
		// Confirm the victim is blocked waiting for the static resource (so a pass
		// through findFirstMissingResourceWhenServiceMutexIsLocked has occurred),
		// then drop the connection so the next iteration gets a fresh pass.
		waitForServiceState(t, managementApiAddress, victimServiceName, ServiceStateWaitingForResources, 3*time.Second)
		_ = victimConn.Close()
		// Let the proxy tear down the victim's reservation before reconnecting.
		time.Sleep(100 * time.Millisecond)
	}

	// Allow any Unpause-poked CheckCommand run (old code) to land, plus margin
	// over the poke -> exec latency.
	time.Sleep(800 * time.Millisecond)

	final := readCounterValue(t, counterFile)
	if final != baseline {
		t.Errorf("CheckCommand ran while the victim was blocked on the static resource: counter went %d -> %d (expected no change). The two-pass fix must evaluate the static resource first and never Unpause the CheckCommand monitor when a static resource is the bottleneck.", baseline, final)
	}
}

// readCounterValue reads the integer stored in the given counter file, returning
// -1 if it is momentarily unparseable (e.g. partially written mid-redirect).
func readCounterValue(t *testing.T, path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read counter file %s: %v", path, err)
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return -1
	}
	return v
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

// TestResourceWaitMaxTimeoutCleanup is a regression test for the max-wait-timeout cleanup path: the
// resource-wait event channels must be cleaned up (deleted from the registry)
// on EVERY exit path in reserveResources — reservation success, max-wait
// timeout, and client disconnect. The client-disconnect path is already covered
// by TestWaitingConnectionCountReleasedWhenClientDisconnects; this test covers
// the max-wait-TIMEOUT exit path.
//
// The integration tests run the proxy as a subprocess, so the in-memory
// registry maps (resourceChangeByResourceChans etc.) cannot be inspected
// directly. Instead this test asserts the observable consequences of correct
// cleanup.
//
// Scenario:
//  1. A HOLDER service starts and keeps a proxied connection (and therefore
//     TestResource) open well beyond the test. Because it has an active proxied
//     connection, canBeStopped reports false, so it cannot be evicted.
//  2. A WAITER connection targets a second service that needs the same
//     resource. It cannot start and is counted as a waiting connection
//     (WaitingConnections == 1).
//  3. The holder cannot be evicted and the resource never frees, so the waiter
//     waits until MaxTimeToWait… expires (set short, 2s, for the test) and then
//     gives up. The max-wait-timeout exit path must delete the waiter's channel
//     registration.
//  4. Assert the waiter aborted on timeout: WaitingConnections drops back to 0
//     and the waiter's client connection is closed by the proxy (Read errors).
//  5. "No stranded waiter" (the real point of this test): close the HOLDER's
//     connection so TestResource frees, then dial the WAITER again with a fresh
//     client. If the timed-out waiter's channel registration had leaked, the
//     fresh start could be affected; a prompt start confirms clean state.
func TestResourceWaitMaxTimeoutCleanup(t *testing.T) {
	t.Parallel()

	const managementApiAddress = "localhost:2170"
	const holderProxyAddress = "localhost:2171"
	const waiterProxyAddress = "localhost:2172"
	const testName = "resource-wait-max-timeout-cleanup"
	const holderServiceName = testName + "_holder"
	const waiterServiceName = testName + "_waiter"

	// Short max-wait so the timeout fires quickly (well under the default 120s).
	maxWait := uint(2)
	maxWaitDuration := time.Duration(maxWait) * time.Second

	// The holder keeps its proxied connection open well beyond the duration of
	// this test (30s) so TestResource stays unavailable to the waiter the whole
	// time, and so the holder is never evictable (canBeStopped == false while it
	// has a proxied connection).
	cfg := Config{
		MaxTimeToWaitForServiceToCloseConnectionBeforeGivingUpSeconds: &maxWait,
		ResourcesAvailable: map[string]ResourceAvailable{"TestResource": {Amount: 1}},
		ManagementApi:      ManagementApi{ListenPort: "2170"},
		Services: []ServiceConfig{
			{
				Name:                 "holder",
				ListenPort:           "2171",
				ProxyTargetHost:      "localhost",
				ProxyTargetPort:      "12171",
				Command:              "./test-server/test-server",
				Args:                 "-p 12171 -sleep-after-writing-pid-duration 30s",
				ResourceRequirements: map[string]int{"TestResource": 1},
			},
			{
				Name:                 "waiter",
				ListenPort:           "2172",
				ProxyTargetHost:      "localhost",
				ProxyTargetPort:      "12172",
				Command:              "./test-server/test-server",
				Args:                 "-p 12172",
				ResourceRequirements: map[string]int{"TestResource": 1},
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
		for _, address := range []string{holderProxyAddress, waiterProxyAddress, managementApiAddress} {
			if err := checkPortClosed(address); err != nil {
				t.Errorf("port %s is still open after application exit: %v", address, err)
			}
		}
	}()

	// 1. Start the holder and keep its connection open so it holds TestResource.
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

	// 3. The waiter cannot get TestResource (held by the non-evictable holder) and
	//    no service can be evicted, so it waits until MaxTimeToWait… expires.
	// 4. Assert the waiter aborted on timeout: WaitingConnections drops back to 0
	//    (its channel registration was cleaned up on the max-wait exit path).
	//
	// The margin comfortably exceeds the short max-wait plus scheduling/process
	// overhead on a loaded machine.
	waitForWaitingConnections(t, managementApiAddress, waiterServiceName, 0, maxWaitDuration+6*time.Second)

	// The waiter's client connection must have been closed by the proxy when it
	// gave up waiting. A Read must return an error (io.EOF or reset). Use a
	// goroutine + select so the test fails fast instead of hanging if the close
	// never arrives.
	readDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, readErr := waiterConn.Read(buf)
		readDone <- readErr
	}()
	select {
	case readErr := <-readDone:
		if readErr == nil {
			t.Fatal("expected the waiter's client connection to be closed by the proxy after the max-wait timeout, but Read returned no error")
		}
		// readErr is io.EOF / connection reset — both indicate the proxy closed it.
	case <-time.After(maxWaitDuration + 6*time.Second):
		t.Fatalf("waiter's client connection was not closed within %v of the max-wait timeout", maxWaitDuration+6*time.Second)
	}
	_ = waiterConn.Close()

	// 5. "No stranded waiter": close the HOLDER's connection so TestResource
	//    frees (the holder becomes evictable once its proxied count hits 0), then
	//    dial the WAITER again with a fresh client. It must start promptly — if
	//    the timed-out waiter's channel registration had leaked/stranded this
	//    fresh start could be affected.
	_ = holderConn.Close()
	// Wait until the holder has released its proxied connection (and is thus
	// evictable), so the prompt-start assertion below is not racing the
	// connection-close accounting.
	waitForProxiedConnections(t, managementApiAddress, holderServiceName, 0, 5*time.Second)

	freshWaiterConn, err := net.DialTimeout("tcp", waiterProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to waiter (fresh): %v", err)
	}
	defer func() { _ = freshWaiterConn.Close() }()
	// The fresh waiter must become Running within a few seconds — confirming the
	// proxy is in clean state after the prior max-wait-timeout exit.
	waitForServiceState(t, managementApiAddress, waiterServiceName, ServiceStateRunning, 8*time.Second)
}

// TestResourceChangeBroadcastOnLastProxiedConnectionClose is a regression test
// for the last-connection-close broadcast. When a service's LAST proxied (or waiting) connection closes,
// incrementConnection broadcasts a resource-change event for that service's
// resources so any waiter is re-evaluated promptly (rather than waiting for the
// holder's idle timeout or the waiter's max-wait timer to fire).
//
// This broadcast is the only thing that lets a waiter unblock within SECONDS of
// a holder's last-close: after the holder's proxied count hits 0 it becomes
// evictable (canBeStopped == true), but without the broadcast nobody would
// re-check resource availability until either the holder idles out (its idle
// timer) or the waiter hits its max-wait. Both of those are configured here to
// be far longer than the asserted window, so a prompt start can only be
// explained by the last-close broadcast.
//
// Setup: a HOLDER holds the single unit of TestResource with a LONG idle timeout
// (300s) and a process that stays up for 60s after writing its PID, so it does
// NOT idle out or exit during the test — it keeps holding TestResource until it
// is evicted. A WAITER requires the same resource and blocks in reserveResources.
// We close the holder's LAST proxied connection and assert the waiter becomes
// Running within a few seconds — only the last-close broadcast (→ waiter
// re-evaluation → holder eviction → TestResource freed and re-reserved) can do
// it that fast. A generous MaxTimeToWait (120s) ensures the waiter is not timing
// out. The holder ends up Stopped (it was evicted).
func TestResourceChangeBroadcastOnLastProxiedConnectionClose(t *testing.T) {
	t.Parallel()

	const managementApiAddress = "localhost:2180"
	const holderProxyAddress = "localhost:2181"
	const waiterProxyAddress = "localhost:2182"
	const testName = "resource-change-broadcast-on-last-proxied-close"
	const holderServiceName = testName + "_holder"
	const waiterServiceName = testName + "_waiter"

	// The holder's idle timeout (300s) is far longer than anything this test
	// waits for, so the holder does NOT idle out after its last proxied conn
	// closes. Its process also stays up for 60s after writing its PID, so it
	// keeps holding TestResource until it is evicted. The waiter's max-wait
	// (120s) is also generous, so the waiter is not timing out during the test.
	holderIdleTimeoutSeconds := uint(300)
	maxWaitSeconds := uint(120)
	cfg := Config{
		MaxTimeToWaitForServiceToCloseConnectionBeforeGivingUpSeconds: &maxWaitSeconds,
		ResourcesAvailable: map[string]ResourceAvailable{"TestResource": {Amount: 1}},
		ManagementApi:      ManagementApi{ListenPort: "2180"},
		Services: []ServiceConfig{
			{
				Name:                           "holder",
				ListenPort:                     "2181",
				ProxyTargetHost:                "localhost",
				ProxyTargetPort:                "12181",
				Command:                        "./test-server/test-server",
				Args:                           "-p 12181 -sleep-after-writing-pid-duration 60s",
				ShutDownAfterInactivitySeconds: holderIdleTimeoutSeconds,
				ResourceRequirements:           map[string]int{"TestResource": 1},
			},
			{
				Name:                 "waiter",
				ListenPort:           "2182",
				ProxyTargetHost:      "localhost",
				ProxyTargetPort:      "12182",
				Command:              "./test-server/test-server",
				Args:                 "-p 12182 -sleep-after-writing-pid-duration 30s",
				ResourceRequirements: map[string]int{"TestResource": 1},
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
		for _, address := range []string{holderProxyAddress, waiterProxyAddress, managementApiAddress} {
			if err := checkPortClosed(address); err != nil {
				t.Errorf("port %s is still open after application exit: %v", address, err)
			}
		}
	}()

	// 1. Start the holder and keep its proxied connection open so it holds TestResource.
	holderConn, err := net.DialTimeout("tcp", holderProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to holder: %v", err)
	}
	defer func() { _ = holderConn.Close() }()
	readPidFromOpenConnection(t, holderConn)
	// holder is now running with one proxied connection, holding TestResource.
	statusResponse := getStatusFromManagementAPI(t, managementApiAddress)
	verifyServiceStatus(t, statusResponse, holderServiceName, ServiceStateRunning, 0, 1, map[string]int{"TestResource": 1})

	// 2. A new connection to the waiter must wait for TestResource (held by the
	//    non-evictable holder, which still has a proxied connection open).
	waiterConn, err := net.DialTimeout("tcp", waiterProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to waiter: %v", err)
	}
	defer func() { _ = waiterConn.Close() }()
	waitForWaitingConnections(t, managementApiAddress, waiterServiceName, 1, 3*time.Second)

	// 3. Close the holder's LAST proxied connection. This must trigger the last-connection-close
	//    broadcast (the holder's proxied AND waiting counts are now both 0). The
	//    broadcast wakes the waiter, which re-evaluates: the holder is now idle
	//    (proxied==0 → canBeStopped true), so the waiter evicts it, frees
	//    TestResource, and reserves it.
	_ = holderConn.Close()

	// 4. The waiter must become Running PROMPTLY. The asserted window (8s) is far
	//    smaller than the holder's idle timeout (300s) AND the waiter's max-wait
	//    (120s), so the ONLY mechanism that can unblock the waiter this fast is
	//    the last-close broadcast → eviction. Without it the waiter would sit
	//    blocked until the holder idles out (300s) or the max-wait fires.
	waitForServiceState(t, managementApiAddress, waiterServiceName, ServiceStateRunning, 8*time.Second)

	// 5. The holder must have been evicted to free TestResource for the waiter.
	// The waiter's waiting connection converts to proxied slightly after the
	// Running state, so poll for proxied==1 (same race as the multi-connection
	// handover test), then confirm the proxied data path by reading the PID the
	// waiter's service wrote back.
	waitForProxiedConnections(t, managementApiAddress, waiterServiceName, 1, 5*time.Second)
	readPidFromOpenConnection(t, waiterConn)
	finalStatus := getStatusFromManagementAPI(t, managementApiAddress)
	verifyServiceStatus(t, finalStatus, holderServiceName, ServiceStateStopped, 0, 0, map[string]int{"TestResource": 0})
	verifyServiceStatus(t, finalStatus, waiterServiceName, ServiceStateRunning, 0, 1, map[string]int{"TestResource": 1})
}

// TestServiceMutexNotHeldDuringSlowCheckCommand pins that, while a
// service waits for a CheckCommand-backed resource whose measurement is in
// flight, the waiter blocks on a channel and the global serviceMutex is NOT held
// for the (potentially slow) duration of the CheckCommand.
//
// handleStatus (the /status management endpoint) acquires serviceMutex, so if
// serviceMutex were held while a slow CheckCommand executed, /status would block
// for the whole duration of that check. This test proves the invariant
// behaviorally: it configures a resource whose CheckCommand is slow AND reports
// an insufficient amount ("sleep 2; echo 0" -> ~2s, 0 available), connects a
// client whose service requires 1 unit (forcing reserveResources to request a
// first CheckCommand run via UnpauseResourceAvailabilityMonitoring and then wait
// on a channel in waitForFirstCheckCommands with serviceMutex released), and
// samples /status latency throughout the ~2s check window. Every sample must
// return in well under the check duration (sub-second) — if any sample
// approaches the ~2s check duration, serviceMutex was held during the check and
// the invariant is violated.
func TestServiceMutexNotHeldDuringSlowCheckCommand(t *testing.T) {
	t.Parallel()

	const managementApiAddress = "localhost:2190"
	const serviceProxyAddress = "localhost:2191"
	const testName = "service-mutex-not-held-during-slow-check"
	const serviceName = testName + "_svc"
	const slowGpu = "SlowGpu"

	// "sleep 2; echo 0" -> the monitor's CheckCommand takes ~2s and reports 0
	// available, so a service requiring 1 unit can never reserve and stays parked
	// in waiting_for_resources. The long CheckWhenNotEnoughIntervalMilliseconds
	// keeps the monitor from re-running the command on a short interval during the
	// test, so the only check in flight while the waiter is blocked is the one
	// triggered by the client's reserveResources.
	const checkCommand = "sleep 2; echo 0"
	checkDuration := 2 * time.Second

	cfg := Config{
		ResourcesAvailable: map[string]ResourceAvailable{
			slowGpu: {
				CheckCommand:                           checkCommand,
				CheckWhenNotEnoughIntervalMilliseconds: 60000,
			},
		},
		LogLevel:      LogLevelDebug,
		ManagementApi: ManagementApi{ListenPort: "2190"},
		Services: []ServiceConfig{
			{
				Name:                 "svc",
				ListenPort:           "2191",
				ProxyTargetHost:      "localhost",
				ProxyTargetPort:      "12191", // need not be reachable: the service never gets past resource reservation
				Command:              "./test-server/test-server",
				Args:                 "-p 12191",
				ResourceRequirements: map[string]int{slowGpu: 1},
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

	// The resource monitor fires its initial CheckCommand immediately at startup
	// (time.NewTimer(0)); that first run also takes ~checkDuration. Wait for it to
	// finish so that the check sampled below is the one triggered by the client's
	// reserveResources (via UnpauseResourceAvailabilityMonitoring), giving a
	// deterministic ~checkDuration window in which the waiter is blocked on the
	// first-check channel with serviceMutex released. startLargeModelProxy already
	// slept ~1s after start, so sleeping another checkDuration lands well past the
	// initial check's completion (~checkDuration after process start).
	time.Sleep(checkDuration)

	// Dial the service proxy port. This triggers handleConnection -> startService
	// -> reserveResources, which registers a first-check channel and pokes the
	// monitor (UnpauseResourceAvailabilityMonitoring) to run the slow CheckCommand,
	// then calls waitForFirstCheckCommands with serviceMutex RELEASED. Keep the
	// conn open so the waiter stays parked in the wait through the sampling window.
	clientConn, err := net.DialTimeout("tcp", serviceProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to service proxy port: %v", err)
	}

	// Sample /status latency throughout the slow CheckCommand window. handleStatus
	// takes serviceMutex, so if serviceMutex were held during the check, each
	// overlapping /status request would block for ~checkDuration. The generous
	// client Timeout is just a safety net; loopback /status normally returns in a
	// few milliseconds.
	statusClient := &http.Client{Timeout: 5 * time.Second}
	const statusLatencyLimit = 500 * time.Millisecond // far under checkDuration (2s); far above normal loopback latency
	const sampleInterval = 200 * time.Millisecond
	sampleWindow := checkDuration + 800*time.Millisecond
	deadline := time.Now().Add(sampleWindow)
	var maxStatusLatency time.Duration
	sampleCount := 0
	for time.Now().Before(deadline) {
		reqStart := time.Now()
		resp, err := statusClient.Get(fmt.Sprintf("http://%s/status", managementApiAddress))
		latency := time.Since(reqStart)
		if err != nil {
			t.Fatalf("/status request failed while the slow CheckCommand was running: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("/status returned status %d during the slow CheckCommand", resp.StatusCode)
		}
		if latency > maxStatusLatency {
			maxStatusLatency = latency
		}
		sampleCount++
		if remain := sampleInterval - time.Since(reqStart); remain > 0 {
			time.Sleep(remain)
		}
	}
	t.Logf("sampled /status %d times over %v while the slow CheckCommand ran; max latency %v (limit %v)",
		sampleCount, sampleWindow, maxStatusLatency, statusLatencyLimit)

	if maxStatusLatency >= statusLatencyLimit {
		t.Errorf(
			"/status was not responsive while a slow CheckCommand ran for a waiting service: max latency %v over %d samples >= limit %v. handleStatus takes serviceMutex, so this means serviceMutex was held during the CheckCommand, violating the invariant that the waiter must block on a channel with serviceMutex released, not under the lock).",
			maxStatusLatency, sampleCount, statusLatencyLimit,
		)
	}

	// Sanity: confirm the wait-on-channel path was actually exercised. The first
	// CheckCommand ran and reported 0 (< 1 required), so the service must be
	// parked in waiting_for_resources — proving the waiter is blocked on a channel
	// (waiting on a channel, not busy-polling) and never reached starting. The timeout covers
	// the check duration plus scheduling margin.
	waitForServiceState(t, managementApiAddress, serviceName, ServiceStateWaitingForResources, checkDuration+2*time.Second)

	// Tear down the waiter cleanly: closing the client unblocks reserveResources
	// on the client-disconnect path so the proxy is in a clean state before the
	// deferred SIGINT, rather than racing a still-blocked waiter.
	_ = clientConn.Close()
	waitForServiceState(t, managementApiAddress, serviceName, ServiceStateStopped, 5*time.Second)
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
