package main

import (
	"net"
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
// Testing that: 1. Service One starts after 6 seconds
//  2. Service Two starts after 11 seconds
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
	verifyTotalResourceUsage(t, statusResponse, map[string]int{resourceName: 0})
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

	assert.Less(t, statusResponse.Resources[resourceName].TotalAvailable, 4, "Resource check ran too many times before the test started")

	maxWaitingTime := 10 * time.Second
	deadline := time.Now().Add(maxWaitingTime)
	for statusResponse.Resources[resourceName].TotalAvailable < 3 {
		//Give lmp time to run the check 3 times.
		//There are sleeps in the init test code, so normally it takes 1.8 s until the
		//code gets here. Giving it 1.2 s buffer to account for possible slowdowns
		time.Sleep(100 * time.Millisecond)
		statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
		if statusResponse.Resources[resourceName].TotalAvailable > 3 {
			t.Fatalf("Failed to catch resource check run exactly 3 times")
			return
		}
		if deadline.Before(time.Now()) {
			t.Fatalf("The attempt to catch resource run 3 times in did not finish in %v", maxWaitingTime)
			return
		}
	}

	verifyTotalResourcesAvailable(t, statusResponse, map[string]int{resourceName: 3})
	verifyTotalResourceUsage(t, statusResponse, map[string]int{resourceName: 0})
	verifyServiceStatus(t, statusResponse, serviceOneName, ServiceStateRunning, 0, 1, map[string]int{resourceName: 4})
	verifyServiceStatus(t, statusResponse, serviceTwoName, ServiceStateWaitingForResources, 1, 0, map[string]int{resourceName: 5})
	assertPortsAreClosed(t, []string{serviceOneHealthCheckAddress, serviceTwoHealthCheckAddress})

	time.Sleep(1000 * time.Millisecond)
	var serviceOneHealthCheckResponse HealthCheckResponse

	serviceOneHealthCheckResponse, err = attemptReadHealthcheckResponse(t, serviceOneHealthCheckAddress)
	statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
	var resourceAvailableAmountExpected = statusResponse.Resources[resourceName].TotalAvailable
	for resourceAvailableAmountExpected < 9 {
		statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
		verifyTotalResourcesAvailable(t, statusResponse, map[string]int{resourceName: resourceAvailableAmountExpected})
		verifyTotalResourceUsage(t, statusResponse, map[string]int{resourceName: 4})
		verifyServiceStatus(t, statusResponse, serviceOneName, ServiceStateRunning, 0, 1, map[string]int{resourceName: 4})
		//service two should not be starting. Even though >=5 total units are available, 4 should be reserved for service one
		verifyServiceStatus(t, statusResponse, serviceTwoName, ServiceStateWaitingForResources, 1, 0, map[string]int{resourceName: 5})
		serviceOneHealthCheckResponse = getHealthcheckResponse(t, serviceOneHealthCheckAddress)
		assert.Equal(t, "server_starting", serviceOneHealthCheckResponse.Message)
		assertPortsAreClosed(t, []string{serviceTwoHealthCheckAddress})
		resourceAvailableAmountExpected++
		time.Sleep(1000 * time.Millisecond)
		//TODO: do we need to sleep more since service two health check can fail still
	}

	statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
	verifyTotalResourcesAvailable(t, statusResponse, map[string]int{resourceName: 9})
	verifyServiceStatus(t, statusResponse, serviceOneName, ServiceStateRunning, 0, 1, map[string]int{resourceName: 4})
	verifyServiceStatus(t, statusResponse, serviceTwoName, ServiceStateRunning, 0, 1, map[string]int{resourceName: 5})
	serviceOneHealthCheckResponse = getHealthcheckResponse(t, serviceOneHealthCheckAddress)
	assert.Equal(t, "server_starting", serviceOneHealthCheckResponse.Message)
	serviceTwoHealthCheckResponse := getHealthcheckResponse(t, serviceTwoHealthCheckAddress)
	assert.Equal(t, "ok", serviceTwoHealthCheckResponse.Message)

	pid := readPidFromOpenConnection(t, connOne)
	assert.True(t, isProcessRunning(pid))
	serviceOneHealthCheckResponse = getHealthcheckResponse(t, serviceOneHealthCheckAddress)
	assert.Equal(t, "ok", serviceOneHealthCheckResponse.Message)
}

// Test resource starts at 10 units, but service one is changing it to 0 units right before its healthcheck is ready.
// Check command runs every 60 seconds, so it won't run on the timer during the duration of the test
// We immediately connect service one and after a second to service two.
// Service one takes 3 seconds to start and then start, until then 11 resources are available
// Connection of service one should trigger check command (we check that).
// Service two is not supposed to start while service one is running.
// It should start immediately after service one terminates since that
// should trigger a check command run (we check that)
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
	verifyTotalResourcesAvailable(t, statusResponse, map[string]int{resourceName: 2})
	verifyTotalResourceUsage(t, statusResponse, map[string]int{resourceName: 0})
	connOne, err := net.Dial("tcp", serviceOneAddress)
	if err != nil {
		t.Fatalf("failed to connect to %s: %v", serviceOneAddress, err)
	}
	serviceOneConnectionEstablishedTime := time.Now()
	defer func() { _ = connOne.Close() }()
	statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
	assertPortsAreClosed(t, []string{serviceOneHealthCheckAddress, serviceTwoHealthCheckAddress})
	//starting the service will set the total resource amount to 11, but the check command should not run again until we receive another request
	verifyServiceStatus(t, statusResponse, serviceOneName, ServiceStateStarting, 1, 0, map[string]int{resourceName: 10})
	verifyServiceStatus(t, statusResponse, serviceTwoName, ServiceStateStopped, 0, 0, map[string]int{resourceName: 0})
	verifyTotalResourcesAvailable(t, statusResponse, map[string]int{resourceName: 10})
	verifyTotalResourceUsage(t, statusResponse, map[string]int{resourceName: 10})

	time.Sleep(1 * time.Second)
	statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
	assertPortsAreClosed(t, []string{serviceOneHealthCheckAddress, serviceTwoHealthCheckAddress})
	verifyServiceStatus(t, statusResponse, serviceOneName, ServiceStateStarting, 1, 0, map[string]int{resourceName: 10})
	verifyServiceStatus(t, statusResponse, serviceTwoName, ServiceStateStopped, 0, 0, map[string]int{resourceName: 0})
	verifyTotalResourcesAvailable(t, statusResponse, map[string]int{resourceName: 10})
	verifyTotalResourceUsage(t, statusResponse, map[string]int{resourceName: 10})

	connTwo, err := net.Dial("tcp", serviceTwoAddress)
	if err != nil {
		t.Fatalf("failed to connect to %s: %v", serviceTwoAddress, err)
	}
	serviceTwoConnectionEstablishedTime := time.Now()
	defer func() { _ = connTwo.Close() }()
	t.Logf("Service two connection established %v after service one", serviceTwoConnectionEstablishedTime.Sub(serviceOneConnectionEstablishedTime))

	time.Sleep(200 * time.Millisecond) //give CheckCommand time to finish running

	statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
	assertPortsAreClosed(t, []string{serviceOneHealthCheckAddress, serviceTwoHealthCheckAddress})
	verifyServiceStatus(t, statusResponse, serviceOneName, ServiceStateStarting, 1, 0, map[string]int{resourceName: 10})

	verifyServiceStatus(t, statusResponse, serviceTwoName, ServiceStateStarting, 1, 0, map[string]int{resourceName: 10})
	verifyTotalResourcesAvailable(t, statusResponse, map[string]int{resourceName: 11})
	verifyTotalResourceUsage(t, statusResponse, map[string]int{resourceName: 10})

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
			t.Fatal("Service on health check is still not responding after 5s")
		}
	}
	statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
	assertPortsAreClosed(t, []string{serviceTwoHealthCheckAddress})
	verifyServiceStatus(t, statusResponse, serviceOneName, ServiceStateRunning, 0, 1, map[string]int{resourceName: 10})
	verifyServiceStatus(t, statusResponse, serviceTwoName, ServiceStateStarting, 1, 0, map[string]int{resourceName: 10})
	verifyTotalResourcesAvailable(t, statusResponse, map[string]int{resourceName: 11})
	verifyTotalResourceUsage(t, statusResponse, map[string]int{resourceName: 10})

	for {
		serviceTwoHealthCheckResponse, err := attemptReadHealthcheckResponse(t, serviceTwoHealthCheckAddress)
		if err == nil {
			assert.Equal(t, "ok", serviceTwoHealthCheckResponse.Message)
			t.Logf("Service two health check response received after %v", time.Since(serviceTwoConnectionEstablishedTime))
			break
		}
		time.Sleep(10 * time.Millisecond)
		if time.Since(serviceTwoConnectionEstablishedTime) > 5*time.Second {
			t.Fatal("Service two health check is still not responding after 5s")
		}
	}
	statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
	assertPortsAreClosed(t, []string{serviceOneHealthCheckAddress})
	verifyServiceStatus(t, statusResponse, serviceOneName, ServiceStateStopped, 0, 0, map[string]int{resourceName: 0})
	verifyServiceStatus(t, statusResponse, serviceTwoName, ServiceStateRunning, 0, 1, map[string]int{resourceName: 10})
	//TODO: after stopping a service run checkcommand, that should fix the assert here
	//TODO: have a version where service is not stopped, but exits on its own?
	verifyTotalResourcesAvailable(t, statusResponse, map[string]int{resourceName: 12})
	verifyTotalResourceUsage(t, statusResponse, map[string]int{resourceName: 10})
	pid := readPidFromOpenConnection(t, connTwo)
	assert.True(t, isProcessRunning(pid))
}
