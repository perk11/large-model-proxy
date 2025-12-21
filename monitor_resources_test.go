package main

import (
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TODO: add debug output option
// testResourceCheckCommand verifies dynamic resource availability updates driven by a check command.
// It connects to the service and observes total resources available and service status over time.
func testResourceCheckCommand(
	t *testing.T, serviceOneAddress string,
	serviceTwoAddress string,
	serviceOneHealthCheckAddress string,
	serviceTwoHealthCheckAddress string,
	serviceOneName string,
	serviceTwoName string,
	managementApiAddress string,
	resourceName string,
) {
	//TODO: make second service need more resource and wait for longer
	time.Sleep(200 * time.Millisecond) // give lmp time to run the check command for the first time
	statusResponse := getStatusFromManagementAPI(t, managementApiAddress)
	assertPortsAreClosed(t, []string{serviceOneHealthCheckAddress, serviceTwoHealthCheckAddress})
	verifyTotalResourcesAvailable(t, statusResponse, map[string]int{resourceName: 1})
	verifyServiceStatus(t, statusResponse, serviceOneName, false, map[string]int{resourceName: 0})
	verifyServiceStatus(t, statusResponse, serviceTwoName, false, map[string]int{resourceName: 0})
	conn, err := net.Dial("tcp", serviceOneAddress)
	if err != nil {
		t.Fatalf("failed to connect to %s: %v", serviceOneAddress, err)
	}
	defer func() { _ = conn.Close() }()
	time.Sleep(2000 * time.Millisecond)
	assertPortsAreClosed(t, []string{serviceOneHealthCheckAddress, serviceTwoHealthCheckAddress})
	statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
	verifyTotalResourcesAvailable(t, statusResponse, map[string]int{resourceName: 2})
	//the check below is weird, but the API already considers the service as running and "using" resources here
	//to be fixed when API is reworked
	verifyServiceStatus(t, statusResponse, serviceOneName, true, map[string]int{resourceName: 3})
	verifyServiceStatus(t, statusResponse, serviceTwoName, false, map[string]int{resourceName: 0})

	time.Sleep(1000 * time.Millisecond)
	assertPortsAreClosed(t, []string{serviceOneHealthCheckAddress, serviceTwoHealthCheckAddress})
	statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
	verifyTotalResourcesAvailable(t, statusResponse, map[string]int{resourceName: 3})
	verifyServiceStatus(t, statusResponse, serviceOneName, true, map[string]int{resourceName: 3})
	verifyServiceStatus(t, statusResponse, serviceTwoName, false, map[string]int{resourceName: 0})

	// since the check whether the resource is available is currently once per second,
	// wait for 1 more iteration to avoid race conditions
	time.Sleep(2000 * time.Millisecond)
	statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
	verifyTotalResourcesAvailable(t, statusResponse, map[string]int{resourceName: 4})
	verifyServiceStatus(t, statusResponse, serviceOneName, true, map[string]int{resourceName: 3})
	verifyServiceStatus(t, statusResponse, serviceTwoName, false, map[string]int{resourceName: 0})
	serviceOneHealthCheckResponse := getHealthcheckResponse(t, serviceOneHealthCheckAddress)
	assert.Equal(t, "ok", serviceOneHealthCheckResponse.Message)

	pid := readPidFromOpenConnection(t, conn)
	assert.True(t, isProcessRunning(pid))
}
