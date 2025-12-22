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
	verifyServiceStatus(t, statusResponse, serviceOneName, false, map[string]int{resourceName: 0})
	verifyServiceStatus(t, statusResponse, serviceTwoName, false, map[string]int{resourceName: 0})
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
	}

	verifyTotalResourcesAvailable(t, statusResponse, map[string]int{resourceName: 3})
	verifyTotalResourceUsage(t, statusResponse, map[string]int{resourceName: 0})
	verifyServiceStatus(t, statusResponse, serviceOneName, true, map[string]int{resourceName: 4})
	verifyServiceStatus(t, statusResponse, serviceTwoName, true, map[string]int{resourceName: 5})
	assertPortsAreClosed(t, []string{serviceOneHealthCheckAddress, serviceTwoHealthCheckAddress})

	time.Sleep(1000 * time.Millisecond)
	var serviceOneHealthCheckResponse HealthCheckResponse
	var resourceAvailableAmountExpected int
	for {
		//The resource is available now, but we need more time for the check to realize this.
		// This can be removed after https://github.com/perk11/large-model-proxy/issues/94 is implemented.
		time.Sleep(100 * time.Millisecond)

		serviceOneHealthCheckResponse, err = attemptReadHealthcheckResponse(t, serviceOneHealthCheckAddress)
		statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
		if resourceAvailableAmountExpected > 5 {
			t.Fatalf("service one should have started by now")
			return
		}
		if err == nil {
			resourceAvailableAmountExpected = statusResponse.Resources[resourceName].TotalAvailable
			assert.Equal(t, "server_starting", serviceOneHealthCheckResponse.Message)
			break
		}
	}

	for resourceAvailableAmountExpected < 9 {
		statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
		verifyTotalResourcesAvailable(t, statusResponse, map[string]int{resourceName: resourceAvailableAmountExpected})
		verifyTotalResourceUsage(t, statusResponse, map[string]int{resourceName: 4})
		verifyServiceStatus(t, statusResponse, serviceOneName, true, map[string]int{resourceName: 4})
		//service two should not be starting. Even though >=5 total units are available, 4 should be reserved for service one
		verifyServiceStatus(t, statusResponse, serviceTwoName, true, map[string]int{resourceName: 5})
		serviceOneHealthCheckResponse = getHealthcheckResponse(t, serviceOneHealthCheckAddress)
		assert.Equal(t, "server_starting", serviceOneHealthCheckResponse.Message)
		assertPortsAreClosed(t, []string{serviceTwoHealthCheckAddress})
		resourceAvailableAmountExpected++
		time.Sleep(1000 * time.Millisecond)
		//TODO: do we need to sleep more since service two health check can fail still
	}

	statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
	verifyTotalResourcesAvailable(t, statusResponse, map[string]int{resourceName: 9})
	verifyServiceStatus(t, statusResponse, serviceOneName, true, map[string]int{resourceName: 4})
	verifyServiceStatus(t, statusResponse, serviceTwoName, true, map[string]int{resourceName: 5})
	serviceOneHealthCheckResponse = getHealthcheckResponse(t, serviceOneHealthCheckAddress)
	assert.Equal(t, "server_starting", serviceOneHealthCheckResponse.Message)
	serviceTwoHealthCheckResponse := getHealthcheckResponse(t, serviceTwoHealthCheckAddress)
	assert.Equal(t, "ok", serviceTwoHealthCheckResponse.Message)

	pid := readPidFromOpenConnection(t, connOne)
	assert.True(t, isProcessRunning(pid))
	serviceOneHealthCheckResponse = getHealthcheckResponse(t, serviceOneHealthCheckAddress)
	assert.Equal(t, "ok", serviceOneHealthCheckResponse.Message)
}

// testResourceCheckCommandShouldNotUseAnOutdatedResourceCheckResult
// Test resource starts at 10 units, but service one is changing it to 0 units.
// Check command runs every 10 seconds.
// We immediately spawn service one and then service two.
// Service two is not supposed to start while service one is running, even though check command hasn't ran yet
// It should start after service one terminates after 15 seconds.
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
	//TODO: implement and fix by changing monitoring to a queue
}
