package main

import (
	"bytes"
	"log"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// testResourceCheckCommand
// The test resource availability increases by 1 with each check which is
// scheduled to be every second.
// Service One needs 4 units of the resource and takes 10 seconds to start
// Service two needs 5 units of the resource
// Both service connections are open at the same time
// Testing that service two starts only once free resources hit 9.
func testResourceCheckCommand(
	t *testing.T,
	serviceOneAddress string,
	serviceTwoAddress string,
	serviceOneHealthCheckAddress string,
	serviceTwoHealthCheckAddress string,
	serviceOneName string,
	serviceTwoName string,
	managementApiAddress string,
	resourceName string,
) {
	var statusResponse StatusResponse
	statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
	assertPortsAreClosed(t, []string{serviceOneHealthCheckAddress, serviceTwoHealthCheckAddress})
	verifyServiceStatus(t, statusResponse, serviceOneName, ServiceStateStopped, 0, 0, map[string]int{resourceName: 0})
	verifyServiceStatus(t, statusResponse, serviceTwoName, ServiceStateStopped, 0, 0, map[string]int{resourceName: 0})
	verifyResourceUsage(t, statusResponse, map[string]int{resourceName: 0}, map[string]int{resourceName: 1}, map[string]int{resourceName: 0}, map[string]int{resourceName: 0})
	connOne, err := net.Dial("tcp", serviceOneAddress)
	if err != nil {
		t.Fatalf("failed to connect to %s: %v", serviceOneAddress, err)
	}
	defer func() { _ = connOne.Close() }()
	connTwo, err := net.Dial("tcp", serviceTwoAddress)
	if err != nil {
		t.Fatalf("failed to connect to %s: %v", serviceTwoAddress, err)
	}
	defer func() { _ = connTwo.Close() }()

	assert.Less(t, statusResponse.Resources[resourceName].Free, 3, "Resource check ran too many times before the main test started")

	maxWaitingTime := 4 * time.Second
	deadline := time.Now().Add(maxWaitingTime)
	for statusResponse.Resources[resourceName].Free < 3 {
		//Give lmp time to run the check 3 times.
		//There are sleeps in the init test code, so normally it takes 1.8 s until the
		//code gets here. Giving it 1.2 s buffer to account for possible slowdowns
		statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
		if statusResponse.Resources[resourceName].Free > 3 {
			t.Fatalf("Failed to catch resource check run exactly 3 times")
			return
		}
		if deadline.Before(time.Now()) {
			t.Fatalf("The attempt to catch resource run 3 times did not finish in %v", maxWaitingTime)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	verifyResourceUsage(t, statusResponse, map[string]int{resourceName: 0}, map[string]int{resourceName: 3}, map[string]int{resourceName: 0}, map[string]int{resourceName: 0})
	verifyServiceStatus(t, statusResponse, serviceOneName, ServiceStateWaitingForResources, 1, 0, map[string]int{resourceName: 4})
	verifyServiceStatus(t, statusResponse, serviceTwoName, ServiceStateWaitingForResources, 1, 0, map[string]int{resourceName: 5})
	assertPortsAreClosed(t, []string{serviceOneHealthCheckAddress, serviceTwoHealthCheckAddress})

	time.Sleep(1000 * time.Millisecond)
	var serviceOneHealthCheckResponse HealthCheckResponse

	//serviceOneHealthCheckResponse, err = attemptReadHealthcheckResponse(t, serviceOneHealthCheckAddress)
	serviceOneHealthCheckResponse = waitForHealthcheckResponse(t, serviceOneHealthCheckAddress, 15*time.Second)
	assert.Equal(t, "server_starting", serviceOneHealthCheckResponse.Message)

	statusResponse = getStatusFromManagementAPI(t, managementApiAddress)

	var resourceFreeAmountExpected = statusResponse.Resources[resourceName].Free
	// The resource CheckCommand is meant to run roughly once per second, bumping
	// the reported free amount by 1 each time. Under load that cadence slips, so
	// poll for the free amount to advance instead of assuming a fixed 1s sleep.
	for resourceFreeAmountExpected < 9 {
		verifyResourceUsage(t, statusResponse, map[string]int{resourceName: 4}, map[string]int{resourceName: resourceFreeAmountExpected}, map[string]int{resourceName: 4}, map[string]int{resourceName: 0})
		verifyServiceStatus(t, statusResponse, serviceOneName, ServiceStateStarting, 1, 0, map[string]int{resourceName: 4})
		//service two should not be starting. Even though >=5 total units are available, 4 should be reserved for service one
		verifyServiceStatus(t, statusResponse, serviceTwoName, ServiceStateWaitingForResources, 1, 0, map[string]int{resourceName: 5})
		serviceOneHealthCheckResponse = waitForHealthcheckResponse(t, serviceOneHealthCheckAddress, 5*time.Second)
		assert.Equal(t, "server_starting", serviceOneHealthCheckResponse.Message)
		assertPortsAreClosed(t, []string{serviceTwoHealthCheckAddress})
		resourceFreeAmountExpected++
		statusResponse = waitForResourceFree(t, managementApiAddress, resourceName, resourceFreeAmountExpected, 5*time.Second)
	}

	statusResponse = waitForServiceState(t, managementApiAddress, serviceTwoName, ServiceStateRunning, 5*time.Second)
	verifyServiceStatus(t, statusResponse, serviceOneName, ServiceStateStarting, 1, 0, map[string]int{resourceName: 4})
	verifyServiceStatus(t, statusResponse, serviceTwoName, ServiceStateRunning, 0, 0, map[string]int{resourceName: 5})
	verifyResourceUsage(t, statusResponse, map[string]int{resourceName: 4}, map[string]int{resourceName: 9}, map[string]int{resourceName: 9}, map[string]int{resourceName: 0})
	serviceOneHealthCheckResponse = getHealthcheckResponse(t, serviceOneHealthCheckAddress)
	assert.Equal(t, "server_starting", serviceOneHealthCheckResponse.Message)
	serviceTwoHealthCheckResponse := getHealthcheckResponse(t, serviceTwoHealthCheckAddress)
	assert.Equal(t, "ok", serviceTwoHealthCheckResponse.Message)

	pid := readPidFromOpenConnection(t, connOne)
	assert.True(t, isProcessRunning(pid))
	serviceOneHealthCheckResponse = getHealthcheckResponse(t, serviceOneHealthCheckAddress)
	assert.Equal(t, "ok", serviceOneHealthCheckResponse.Message)
	statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
	verifyServiceStatus(t, statusResponse, serviceOneName, ServiceStateRunning, 0, 0, map[string]int{resourceName: 4})
	verifyServiceStatus(t, statusResponse, serviceTwoName, ServiceStateRunning, 0, 0, map[string]int{resourceName: 5})
	verifyResourceUsage(t, statusResponse, map[string]int{resourceName: 0}, map[string]int{resourceName: 10}, map[string]int{resourceName: 9}, map[string]int{resourceName: 0})
}

// Test resource starts at 10 units, but service one is changing it to 0 units right before its healthcheck is ready.
// Check command runs every 60 seconds, so it won't run on the timer during the duration of the test
// We immediately connect service one and after a second to service two.
// Service one takes 3 seconds to start and then start, until then 11 resources are available
// Connection of service one should trigger check command (we check that).
// Service two is not supposed to start while service one is running.
// It should start immediately after service one terminates since that
// should trigger a check command run (we check that)
//
// This is also the regression test that exposed the dropped-broadcast bug fixed
// in commit f6188c2: service two only starts if it receives the resource-change
// broadcast that service one's termination fires while service two is waiting
// (and re-registering) for it. Under the CI's -parallel 500 that broadcast used
// to land in service two's register→select window and get dropped by the
// non-blocking send, leaving service two stuck. See sendSignalToChannels and
// TestSendSignalToChannelsRequiresBufferedChannels for the invariant.
func testResourceCheckCommandShouldNotUseAnOutdatedResourceCheckResult(
	t *testing.T,
	serviceOneAddress string,
	serviceTwoAddress string,
	serviceOneHealthCheckAddress string,
	serviceTwoHealthCheckAddress string,
	serviceOneName string,
	serviceTwoName string,
	managementApiAddress string,
	resourceName string,
) {
	var statusResponse StatusResponse
	statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
	assertPortsAreClosed(t, []string{serviceOneHealthCheckAddress, serviceTwoHealthCheckAddress})
	verifyServiceStatus(t, statusResponse, serviceOneName, ServiceStateStopped, 0, 0, map[string]int{resourceName: 0})
	verifyServiceStatus(t, statusResponse, serviceTwoName, ServiceStateStopped, 0, 0, map[string]int{resourceName: 0})
	verifyResourceUsage(t, statusResponse, map[string]int{resourceName: 0}, map[string]int{resourceName: 10}, map[string]int{resourceName: 0}, map[string]int{resourceName: 2})

	connOne, err := net.Dial("tcp", serviceOneAddress)
	if err != nil {
		t.Fatalf("failed to connect to %s: %v", serviceOneAddress, err)
	}
	serviceOneConnectionEstablishedTime := time.Now()
	defer func() { _ = connOne.Close() }()

	// Give lmp time to process the connection and run the resource CheckCommand
	// before checking. Poll for service0 to start reserving resources rather
	// than relying on a fixed sleep that races on slow/loaded machines.
	statusResponse = waitForServiceState(t, managementApiAddress, serviceOneName, ServiceStateStarting, 5*time.Second)
	assertPortsAreClosed(t, []string{serviceOneHealthCheckAddress, serviceTwoHealthCheckAddress})
	//starting the service will set the total resource amount to 11, but the check command should not run again until we receive another request
	verifyServiceStatus(t, statusResponse, serviceOneName, ServiceStateStarting, 1, 0, map[string]int{resourceName: 10})
	verifyServiceStatus(t, statusResponse, serviceTwoName, ServiceStateStopped, 0, 0, map[string]int{resourceName: 0})
	//total never changes from 2
	verifyResourceUsage(t, statusResponse, map[string]int{resourceName: 10}, map[string]int{resourceName: 10}, map[string]int{resourceName: 10}, map[string]int{resourceName: 2})

	time.Sleep(1 * time.Second)
	statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
	assertPortsAreClosed(t, []string{serviceOneHealthCheckAddress, serviceTwoHealthCheckAddress})
	verifyServiceStatus(t, statusResponse, serviceOneName, ServiceStateStarting, 1, 0, map[string]int{resourceName: 10})
	verifyServiceStatus(t, statusResponse, serviceTwoName, ServiceStateStopped, 0, 0, map[string]int{resourceName: 0})
	verifyResourceUsage(t, statusResponse, map[string]int{resourceName: 10}, map[string]int{resourceName: 10}, map[string]int{resourceName: 10}, map[string]int{resourceName: 2})

	connTwo, err := net.Dial("tcp", serviceTwoAddress)
	if err != nil {
		t.Fatalf("failed to connect to %s: %v", serviceTwoAddress, err)
	}
	serviceTwoConnectionEstablishedTime := time.Now()
	defer func() { _ = connTwo.Close() }()
	t.Logf("Service two connection established %v after service one", serviceTwoConnectionEstablishedTime.Sub(serviceOneConnectionEstablishedTime))

	// service1 reaches WaitingForResources once its connection is processed,
	// and the resource CheckCommand triggered by that connection refreshes the
	// reported amount (to 11, after service0 wrote it). Poll for both rather
	// than a fixed sleep: the CheckCommand refresh is asynchronous.
	waitForWaitingConnections(t, managementApiAddress, serviceTwoName, 1, 5*time.Second)
	waitForResourceFree(t, managementApiAddress, resourceName, 11, 5*time.Second)
	statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
	assertPortsAreClosed(t, []string{serviceOneHealthCheckAddress, serviceTwoHealthCheckAddress})
	verifyServiceStatus(t, statusResponse, serviceOneName, ServiceStateStarting, 1, 0, map[string]int{resourceName: 10})

	verifyServiceStatus(t, statusResponse, serviceTwoName, ServiceStateWaitingForResources, 1, 0, map[string]int{resourceName: 10})
	verifyResourceUsage(t, statusResponse, map[string]int{resourceName: 10}, map[string]int{resourceName: 11}, map[string]int{resourceName: 10}, map[string]int{resourceName: 2})

	assertPortsAreClosed(t, []string{serviceOneHealthCheckAddress})
	for {
		serviceOneHealthCheckResponse, err := attemptReadHealthcheckResponse(t, serviceOneHealthCheckAddress)
		if err == nil {
			assert.Equal(t, "ok", serviceOneHealthCheckResponse.Message)
			t.Logf("Service one health check response received after %v", time.Since(serviceOneConnectionEstablishedTime))
			break
		}
		time.Sleep(10 * time.Millisecond)
		if time.Since(serviceOneConnectionEstablishedTime) > 5*time.Second {
			t.Fatal("Service one health check is still not responding after 5s")
		}
	}
	// Once service one is ready its reservation exceeds the now-diminished
	// resource, so the proxy stops it (running its exit script, which bumps the
	// resource file to 12) and service two starts in its place. Poll for service
	// two to reach "running" instead of relying on a fixed sleep: the handover
	// timing varies and a single check races on slow/loaded machines.
	statusResponse = waitForServiceState(t, managementApiAddress, serviceTwoName, ServiceStateRunning, 5*time.Second)
	verifyServiceStatus(t, statusResponse, serviceOneName, ServiceStateStopped, 0, 0, nil)
	verifyServiceStatus(t, statusResponse, serviceTwoName, ServiceStateRunning, 0, 0, map[string]int{resourceName: 10})
	verifyResourceUsage(t, statusResponse, map[string]int{resourceName: 0}, map[string]int{resourceName: 12}, map[string]int{resourceName: 10}, map[string]int{resourceName: 2})
	assertPortsAreClosed(t, []string{serviceOneHealthCheckAddress})
	pid := readPidFromOpenConnection(t, connTwo)
	assert.True(t, isProcessRunning(pid))
}

// Test that when a resource CheckCommand always reports insufficient resources,
// the connection attempt times out and the service never starts.
func testResourceCheckCommandMaxWaitTimeTimeout(
	t *testing.T,
	serviceAddress string,
	serviceHealthCheckAddress string,
	serviceName string,
	managementApiAddress string,
	resourceName string,
	maxWaitSeconds int,
) {
	statusResponse := getStatusFromManagementAPI(t, managementApiAddress)
	verifyServiceStatus(t, statusResponse, serviceName, ServiceStateStopped, 0, 0, map[string]int{resourceName: 0})
	assertPortsAreClosed(t, []string{serviceHealthCheckAddress})

	// Connect to the proxy — the service requires resources that are never available.
	// The CheckCommand always returns 0, so resources are never sufficient.
	// After maxWaitSeconds, the proxy should give up and close the connection.
	connectStart := time.Now()
	conn, err := net.DialTimeout("tcp", serviceAddress, time.Duration(maxWaitSeconds+5)*time.Second)
	if err == nil {
		defer func() { _ = conn.Close() }()
		// Connection succeeded but should be closed by the proxy after timeout.
		// The read deadline is a safety net comfortably above maxWaitSeconds so
		// that we detect the proxy's actual connection close (which happens only
		// after it has given up waiting for resources), rather than racing the
		// deadline against the proxy's own maxWait timer.
		conn.SetReadDeadline(time.Now().Add(time.Duration(maxWaitSeconds+3) * time.Second))
		buf := make([]byte, 1)
		_, readErr := conn.Read(buf)
		if readErr == nil {
			t.Fatal("expected connection to be closed after max wait timeout, but it is still open")
		}
	}
	connectDuration := time.Since(connectStart)
	t.Logf("Connection attempt took %v (expected ~%ds)", connectDuration, maxWaitSeconds)

	// Verify the connection was refused or closed within the expected timeout window.
	// Allow some buffer for scheduling and process startup overhead.
	expectedMin := time.Duration(maxWaitSeconds)*time.Second - 2*time.Second
	expectedMax := time.Duration(maxWaitSeconds)*time.Second + 4*time.Second
	if connectDuration < expectedMin {
		t.Errorf("Connection attempt took %v, expected at least %v", connectDuration, expectedMin)
	}
	if connectDuration > expectedMax {
		t.Errorf("Connection attempt took %v, expected at most %v", connectDuration, expectedMax)
	}

	// Service must remain stopped — it should never have started.
	statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
	verifyServiceStatus(t, statusResponse, serviceName, ServiceStateStopped, 0, 0, map[string]int{resourceName: 0})
	// Resources were never reserved (timeout), so InUse and Reserved are 0.
	// Total is 0 (no Amount configured), Free is 0 (CheckCommand returns "0").
	verifyResourceUsage(t, statusResponse, map[string]int{resourceName: 0}, map[string]int{resourceName: 0}, map[string]int{resourceName: 0}, map[string]int{resourceName: 0})
	assertPortsAreClosed(t, []string{serviceHealthCheckAddress})
}

// TestSendSignalToChannelsRequiresBufferedChannels documents and locks in the
// load-bearing invariant that sendSignalToChannels relies on: it fans a signal
// out to listeners with a NON-BLOCKING send (select with a default branch), so
// every channel it targets MUST be buffered (capacity >= 1).
//
// Why that matters: a consumer registers its channel, then does a little work
// before entering the select that receives from it. A broadcast landing in that
// register→select window has to be buffered; with an unbuffered channel the
// non-blocking send drops it (logged as "... channel is blocked") and the
// consumer hangs until its maxWaitTime deadline. That was the root cause of a
// service never starting after a peer freed resources — see commit f6188c2 and
// the comment on sendSignalToChannels. All broadcast targets
// (resourceChangeByResourceChans, checkCommandFirstChangeByResourceChans and
// monitorUnpauseChans) are created with capacity 1; this test expresses that
// invariant as code rather than just prose.
func TestSendSignalToChannelsRequiresBufferedChannels(t *testing.T) {
	// Buffered target (capacity 1): the signal survives even though no receiver
	// is ready — this is the property every real broadcast target relies on.
	buffered := make(chan bool, 1)
	sendSignalToChannels(map[string]chan bool{"svc": buffered}, "TestResource", "test", true)
	select {
	case v := <-buffered:
		assert.True(t, v, "buffered channel should have received the signal")
	default:
		t.Fatal("buffered channel dropped the signal; sendSignalToChannels targets must be buffered so broadcasts in the register→select window are not lost")
	}

	// Unbuffered target (the bug): with no receiver currently selecting, the
	// non-blocking send drops the signal. This is exactly why an unbuffered
	// channel must never be used as a sendSignalToChannels target.
	unbuffered := make(chan bool)
	sendSignalToChannels(map[string]chan bool{"svc": unbuffered}, "TestResource", "test", true)
	select {
	case <-unbuffered:
		t.Fatal("unbuffered channel should have dropped the signal when no receiver was ready")
	default:
		// expected: the non-blocking send was dropped
	}
}

// TestIncrementConnectionDoesNotPanicOnUnknownService guards the "both counters
// reached zero" branch of incrementConnection. When the last connection to a
// service closes, that branch broadcasts a resource-change event for the
// service's declared resources, dereferencing serviceConfigByName[name].
//
// For an unknown/stale/renamed name, serviceConfigByName[name] is nil, and the
// unguarded nil.ResourceRequirements deref used to panic the proxy. The branch
// is latent today (every caller passes a valid name), but a single future
// caller passing a stale name would crash it. This test passes 0/0 deltas for a
// name that is absent from both connectionStats and serviceConfigByName: the
// zero-value ServiceConnectionStats gives proxied==0 && waiting==0, which takes
// the broadcast branch and exercises the previously-unguarded deref. The fix
// skips the broadcast when the config is missing; we assert no panic.
func TestIncrementConnectionDoesNotPanicOnUnknownService(t *testing.T) {
	rm := ResourceManager{
		connectionStatsMutex:                   &sync.Mutex{},
		connectionStats:                        map[string]ServiceConnectionStats{},
		resourceChangeByResourceMutex:          &sync.Mutex{},
		resourceChangeByResourceChans:          map[string]map[string]chan bool{},
		checkCommandFirstChangeByResourceChans: map[string]map[string]chan struct{}{},
	}

	// "definitely-not-a-real-service" is intentionally absent from both
	// connectionStats and serviceConfigByName, so the broadcast branch hits the
	// nil-config path. 0/0 deltas leave the zero-value stats at 0/0.
	assert.NotPanics(t, func() {
		rm.incrementConnection("definitely-not-a-real-service", 0, 0)
	})
}

// TestIncrementConnectionClampsNegativeCounter guards against a negative
// connection counter must not corrupt state. The previous implementation logged
// a negative value but left it persisted in connectionStats[name], so a later
// increment would build on the negative base instead of on 0. This test drives
// the proxied counter negative with an unmatched decrement, asserts it is
// clamped to 0 (not left at -1), then asserts a subsequent increment yields 1
// (built on the clamped 0, proving no corruption). It also captures the log to
// confirm the "Negative connection count" error is emitted.
//
// "svc" is intentionally absent from both connectionStats and
// serviceConfigByName so the broadcast branch's ok/nil guard makes it a no-op,
// isolating the clamp behaviour.
func TestIncrementConnectionClampsNegativeCounter(t *testing.T) {
	rm := ResourceManager{
		connectionStatsMutex:                   &sync.Mutex{},
		connectionStats:                        map[string]ServiceConnectionStats{},
		resourceChangeByResourceMutex:          &sync.Mutex{},
		resourceChangeByResourceChans:          map[string]map[string]chan bool{},
		checkCommandFirstChangeByResourceChans: map[string]map[string]chan struct{}{},
	}

	// Capture the standard log into a buffer. global log.SetOutput is racy with
	// parallel tests, so this test deliberately does NOT call t.Parallel() and
	// restores the previous output in t.Cleanup.
	var logBuf bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(prevOut) })

	// 1. Drive proxied to -1; it must be clamped to 0 before being persisted.
	assert.NotPanics(t, func() {
		rm.incrementConnection("svc", -1, 0)
	})
	rm.connectionStatsMutex.Lock()
	assert.Equal(t, 0, rm.connectionStats["svc"].proxied, "negative proxied counter should be clamped to 0, not persisted")
	assert.Equal(t, 0, rm.connectionStats["svc"].waiting)
	rm.connectionStatsMutex.Unlock()

	// 2. A subsequent increment must build on the clamped 0, not on -1.
	rm.incrementConnection("svc", 1, 0)
	rm.connectionStatsMutex.Lock()
	assert.Equal(t, 1, rm.connectionStats["svc"].proxied, "increment after clamp must build on 0, not on the prior negative value")
	assert.Equal(t, 0, rm.connectionStats["svc"].waiting)
	rm.connectionStatsMutex.Unlock()

	// 3. The clamp path logs a "Negative connection count" error.
	assert.Contains(t, logBuf.String(), "Negative connection count")
}

// TestCheckCommandMonitorKeepsPollingWhileWaiterWaits is the focused regression
// test for the monitor-stops-polling race fixed in commit 1e57cfd.
//
// monitorResourceAvailability re-armed its CheckCommand timer only while the
// waiter set was non-empty, but it read that set in a separate critical section
// run AFTER broadcasting the result. A waiter deregisters itself on receiving a
// broadcast and only re-registers after re-evaluating, so the waiter set is
// transiently empty; when the monitor's read landed in that window it saw zero
// waiters, did not re-arm the timer, and STOPPED running the CheckCommand. The
// cached free amount then stalled and any waiter on that resource hung until its
// MaxTimeToWait (120s) — the intermittent resource-check-command failure under
// load. The fix captures "did this check have a waiter" under the broadcast lock
// (hadListeners) and drives the re-arm from that.
//
// This test pins the post-fix invariant directly and fast: one service is parked
// in waiting_for_resources on an insufficient CheckCommand resource whose check
// increments a counter (so each run strictly raises the reported free amount).
// While the waiter stays registered the monitor MUST keep polling on its
// interval, so the reported free amount strictly increases across several
// consecutive check intervals. If the monitor ever stops re-arming (the
// regressed behavior), free stalls and the per-step timeout fails the test in
// seconds instead of hanging for MaxTimeToWait.
func TestCheckCommandMonitorKeepsPollingWhileWaiterWaits(t *testing.T) {
	t.Parallel()

	const (
		managementApiAddress = "localhost:2200"
		serviceProxyAddress  = "localhost:2201"
		testName             = "check-command-monitor-keeps-polling"
		serviceName          = testName + "_svc"
		resourceName         = "TestResource"
		// Unique counter file (relative to the proxy's working dir, like the
		// resource-check-command scenario) so this does not clash with that test
		// running in parallel.
		counterFile = "test-logs/check-command-monitor-keeps-polling.counter.txt"
		// Requirement is far above anything the counter reaches during the test,
		// so the service stays parked in waiting_for_resources the whole time.
		requirement = 100
		// Short interval so the test runs in a few seconds.
		checkIntervalMs = 300
	)

	// Start the counter fresh (mirrors the resource-check-command SetupFunc).
	// Ensure test-logs/ exists: it is normally created by startLargeModelProxy,
	// but this test writes the counter file before starting the proxy.
	if err := os.MkdirAll("test-logs", 0755); err != nil {
		t.Fatalf("could not create test-logs directory: %v", err)
	}
	if err := os.Remove(counterFile); err != nil && !os.IsNotExist(err) {
		t.Fatalf("could not remove stale counter file: %v", err)
	}
	if err := os.WriteFile(counterFile, []byte("0"), 0644); err != nil {
		t.Fatalf("could not write counter file: %v", err)
	}

	// Long max-wait and startup timeouts so the parked waiter never times out
	// during the (few-second) observation window.
	maxWaitSeconds := uint(60)
	startupTimeoutMs := uint(60_000)

	cfg := Config{
		MaxTimeToWaitForServiceToCloseConnectionBeforeGivingUpSeconds: &maxWaitSeconds,
		ResourcesAvailable: map[string]ResourceAvailable{
			resourceName: {
				// Each run reads the counter, increments by 1, and echoes it — so
				// the reported free amount strictly increases by 1 per check.
				CheckCommand:                           "read -r original_integer < " + counterFile + "; incremented_integer=$((original_integer + 1)); printf '%d\\n' \"$incremented_integer\" | tee " + counterFile,
				CheckWhenNotEnoughIntervalMilliseconds: checkIntervalMs,
			},
		},
		LogLevel:      LogLevelDebug,
		ManagementApi: ManagementApi{ListenPort: "2200"},
		Services: []ServiceConfig{
			{
				Name:                       "svc",
				ListenPort:                 "2201",
				ProxyTargetHost:            "localhost",
				ProxyTargetPort:            "12201", // need not be reachable: the service never gets past resource reservation
				Command:                    "./test-server/test-server",
				Args:                       "-p 12201",
				StartupTimeoutMilliseconds: &startupTimeoutMs,
				ResourceRequirements:       map[string]int{resourceName: requirement},
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

	// Dial the service proxy port. This triggers handleConnection -> startService
	// -> reserveResources, which registers a first-check channel, pokes the
	// monitor (UnpauseResourceAvailabilityMonitoring) to run the CheckCommand,
	// and then parks the waiter until enough resources are reported. Keep the
	// conn open so the waiter stays registered for the whole observation window.
	clientConn, err := net.DialTimeout("tcp", serviceProxyAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to service proxy port: %v", err)
	}
	defer func() { _ = clientConn.Close() }()

	// Confirm the service is parked in waiting_for_resources, i.e. a waiter is
	// registered for the resource (the precondition for the monitor re-arming).
	resp := waitForServiceState(t, managementApiAddress, serviceName, ServiceStateWaitingForResources, 3*time.Second)

	// Now assert the invariant: while the waiter stays registered, the monitor
	// keeps polling every checkInterval, so the reported free amount strictly
	// increases across several consecutive intervals. waitForFreeIncrease fails
	// fast (per-step timeout) if free ever stalls — exactly the regression
	// signal that the monitor stopped re-arming its timer.
	const (
		numIncrementsToObserve = 5
		// >> checkInterval (300ms); comfortable on a loaded CI but still fails
		// fast instead of hanging for the 60s MaxTimeToWait.
		perStepTimeout = 3 * time.Second
	)

	waitForFreeIncrease := func(prev int) int {
		t.Helper()
		deadline := time.Now().Add(perStepTimeout)
		for {
			r := getStatusFromManagementAPI(t, managementApiAddress)
			info, ok := r.Resources[resourceName]
			if !ok {
				t.Fatalf("resource %s not found in status response", resourceName)
			}
			if info.Free > prev {
				return info.Free
			}
			if time.Now().After(deadline) {
				t.Fatalf(
					"resource %s free stalled at %d for %s — the CheckCommand monitor stopped re-arming its timer while a waiter was still registered (regression for commit 1e57cfd)",
					resourceName, prev, perStepTimeout,
				)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	lastFree := resp.Resources[resourceName].Free
	t.Logf("service parked in waiting_for_resources; starting from free=%d, expecting %d strict increases", lastFree, numIncrementsToObserve)
	for i := 0; i < numIncrementsToObserve; i++ {
		newFree := waitForFreeIncrease(lastFree)
		// On a quiet box each step is exactly +1, but under load two checks can
		// land close together, so assert the load-bearing invariant (strictly
		// increasing) rather than an exact +1 delta.
		assert.Greater(t, newFree, lastFree, "free must strictly increase across consecutive check intervals (step %d)", i+1)
		t.Logf("check interval %d: free %d -> %d", i+1, lastFree, newFree)
		lastFree = newFree
	}
}
