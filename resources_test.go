package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

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
