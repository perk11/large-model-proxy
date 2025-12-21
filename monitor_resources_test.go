package main

import (
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TODO: Implement check commands for each process that overrides resources used
// TODO: there is a bug where none of the resources are actually considered available
// TODO: add debug output option
// testResourceCheckCommand verifies dynamic resource availability updates driven by a check command.
// It connects to the service and observes total resources available and service status over time.
func testResourceCheckCommand(t *testing.T, serviceAddress string, managementApiAddress string, resourceName string, serviceName string) {
	//TODO: add second service
	time.Sleep(200 * time.Millisecond) // give lmp time to run the check command for the first time
	statusResponse := getStatusFromManagementAPI(t, managementApiAddress)
	verifyTotalResourcesAvailable(t, statusResponse, map[string]int{resourceName: 1})
	verifyServiceStatus(t, statusResponse, serviceName, false, map[string]int{resourceName: 0})
	conn, err := net.Dial("tcp", serviceAddress)
	if err != nil {
		t.Fatalf("failed to connect to %s: %v", serviceAddress, err)
	}
	defer func() { _ = conn.Close() }()
	time.Sleep(2000 * time.Millisecond)
	statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
	verifyTotalResourcesAvailable(t, statusResponse, map[string]int{resourceName: 2})
	//todo: add healthcheck checks since API is already considering the resources as used
	verifyServiceStatus(t, statusResponse, serviceName, true, map[string]int{resourceName: 0})

	time.Sleep(2000 * time.Millisecond)
	statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
	verifyTotalResourcesAvailable(t, statusResponse, map[string]int{resourceName: 3})
	verifyServiceStatus(t, statusResponse, serviceName, true, map[string]int{resourceName: 0})

	// since the check whether the resource is available is currently once per second,
	// wait for 1 more iteration to avoid race conditions
	time.Sleep(2000 * time.Millisecond)
	statusResponse = getStatusFromManagementAPI(t, managementApiAddress)
	verifyTotalResourcesAvailable(t, statusResponse, map[string]int{resourceName: 4})
	verifyServiceStatus(t, statusResponse, serviceName, true, map[string]int{resourceName: 3})

	pid := readPidFromOpenConnection(t, conn)
	assert.True(t, isProcessRunning(pid))
}
