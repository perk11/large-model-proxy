package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// StatusResponse represents the complete status response from the management API
type StatusResponse struct {
	AllServices     []ServiceStatus          `json:"all_services"`
	RunningServices []ServiceStatus          `json:"running_services"`
	Resources       map[string]ResourceUsage `json:"resources"`
}

// ServiceStatus represents the current state of a service
type ServiceStatus struct {
	Name                 string         `json:"name"`
	ListenPort           string         `json:"listen_port"`
	IsRunning            bool           `json:"is_running"`
	ActiveConnections    int            `json:"active_connections"`
	LastUsed             *time.Time     `json:"last_used"`
	ResourceRequirements map[string]int `json:"resource_requirements"`
}

// ResourceUsage represents the current usage of a resource
type ResourceUsage struct {
	TotalAvailable int            `json:"total_available"`
	TotalInUse     int            `json:"total_in_use"`
	UsageByService map[string]int `json:"usage_by_service"`
}

func getStatusFromManagementAPI(t *testing.T) StatusResponse {
	resp, err := http.Get("http://localhost:2040/status")
	if err != nil {
		t.Fatalf("Failed to get status from management API: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected status code 200, got %d", resp.StatusCode)
	}

	var statusResp StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&statusResp); err != nil {
		t.Fatalf("Failed to decode status response: %v", err)
	}

	return statusResp
}

// verifyServiceStatus checks if a specific service has the expected running status and resource usage
func verifyServiceStatus(t *testing.T, resp StatusResponse, serviceName string, expectedRunning bool, expectedResources map[string]int) {
	// Find service in allServices
	var found bool
	var service ServiceStatus

	for _, s := range resp.AllServices {
		if s.Name == serviceName {
			service = s
			found = true
			break
		}
	}

	if !found {
		t.Fatalf("Service %s not found in AllServices", serviceName)
	}

	// Check running status
	if service.IsRunning != expectedRunning {
		t.Errorf("Service %s - expected running: %v, actual: %v", serviceName, expectedRunning, service.IsRunning)
	}

	// Check if service is in running services if it should be running
	foundInRunning := false
	for _, s := range resp.RunningServices {
		if s.Name == serviceName {
			foundInRunning = true
			break
		}
	}

	if expectedRunning && !foundInRunning {
		t.Errorf("Service %s expected to be in RunningServices but not found", serviceName)
	} else if !expectedRunning && foundInRunning {
		t.Errorf("Service %s not expected to be in RunningServices but was found", serviceName)
	}

	// Check resource usage
	if expectedRunning {
		for resource, expectedAmount := range expectedResources {
			resourceInfo, ok := resp.Resources[resource]
			if !ok {
				t.Errorf("Resource %s not found in status response", resource)
				continue
			}

			actualAmount, ok := resourceInfo.UsageByService[serviceName]
			if !ok {
				t.Errorf("Service %s not found in UsageByService for resource %s", serviceName, resource)
				continue
			}

			if actualAmount != expectedAmount {
				t.Errorf("Service %s - expected %s usage: %d, actual: %d",
					serviceName, resource, expectedAmount, actualAmount)
			}
		}
	}
}

// verifyTotalResourceUsage checks if the total resource usage matches the expected values
func verifyTotalResourceUsage(t *testing.T, resp StatusResponse, expectedUsage map[string]int) {
	for resource, expectedAmount := range expectedUsage {
		resourceInfo, ok := resp.Resources[resource]
		if !ok {
			t.Errorf("Resource %s not found in status response", resource)
			continue
		}

		if resourceInfo.TotalInUse != expectedAmount {
			t.Errorf("Expected total %s usage: %d, actual: %d",
				resource, expectedAmount, resourceInfo.TotalInUse)
		}
	}
}

func TestManagementAPIStatusAcrossServices(t *testing.T) {
	// Setup test environment
	waitChannel := make(chan error, 1)

	cfg := Config{
		ShutDownAfterInactivitySeconds: 4,
		ResourcesAvailable: map[string]int{
			"CPU": 4,
			"GPU": 2,
		},
		ManagementApi: ManagementApi{
			ListenPort: "2040",
		},
		Services: []ServiceConfig{
			{
				Name:            "service1-cpu",
				ListenPort:      "2041",
				ProxyTargetHost: "localhost",
				ProxyTargetPort: "12041",
				Command:         "./test-server/test-server",
				Args:            "-p 12041",
				LogFilePath:     "test-logs/test-server_service1-cpu.log",
				ResourceRequirements: map[string]int{
					"CPU": 2,
				},
			},
			{
				Name:            "service2-gpu",
				ListenPort:      "2042",
				ProxyTargetHost: "localhost",
				ProxyTargetPort: "12042",
				Command:         "./test-server/test-server",
				Args:            "-p 12042",
				LogFilePath:     "test-logs/test-server_service2-gpu.log",
				ResourceRequirements: map[string]int{
					"GPU": 1,
				},
			},
			{
				Name:            "service3-cpu-gpu",
				ListenPort:      "2043",
				ProxyTargetHost: "localhost",
				ProxyTargetPort: "12043",
				Command:         "./test-server/test-server",
				Args:            "-p 12043",
				LogFilePath:     "test-logs/test-server_service3-cpu-gpu.log",
				ResourceRequirements: map[string]int{
					"CPU": 2,
					"GPU": 1,
				},
			},
		},
	}

	configFilePath := createTempConfig(t, cfg)

	// Start large-model-proxy with our test configuration
	cmd, err := startLargeModelProxy("management-api-test", configFilePath, waitChannel)
	if err != nil {
		t.Fatalf("Could not start application: %v", err)
	}

	defer func() {
		if err := stopApplication(cmd, waitChannel); err != nil {
			t.Errorf("Failed to stop application: %v", err)
		}

		// Verify all ports are closed after stopping
		addresses := []string{
			"localhost:2040", "localhost:2041", "localhost:2042", "localhost:2043",
			"localhost:12041", "localhost:12042", "localhost:12043",
		}
		for _, address := range addresses {
			if err := checkPortClosed(address); err != nil {
				t.Errorf("Port %s is still open after application exit", address)
			}
		}
	}()

	// Give the management API time to start
	time.Sleep(2 * time.Second)

	// Initial check - no services should be running
	t.Log("Checking initial status - no services should be running")
	resp := getStatusFromManagementAPI(t)

	if len(resp.RunningServices) != 0 {
		t.Errorf("Expected 0 running services, got %d", len(resp.RunningServices))
	}

	verifyTotalResourceUsage(t, resp, map[string]int{
		"CPU": 0,
		"GPU": 0,
	})

	// Activate Service 1 (CPU)
	t.Log("Activating Service 1 (CPU)")
	pid1 := runReadPidCloseConnection(t, "localhost:2041")
	if pid1 == 0 {
		t.Fatal("Failed to start service1-cpu")
	}

	// Wait for status to update
	time.Sleep(1000 * time.Millisecond)

	// Check status after starting Service 1
	resp = getStatusFromManagementAPI(t)
	verifyServiceStatus(t, resp, "service1-cpu", true, map[string]int{"CPU": 2})
	verifyTotalResourceUsage(t, resp, map[string]int{
		"CPU": 2,
		"GPU": 0,
	})

	// Activate Service 2 (GPU)
	t.Log("Activating Service 2 (GPU)")
	pid2 := runReadPidCloseConnection(t, "localhost:2042")
	if pid2 == 0 {
		t.Fatal("Failed to start service2-gpu")
	}

	// Wait for status to update
	time.Sleep(1000 * time.Millisecond)

	// Check status after starting Service 2
	resp = getStatusFromManagementAPI(t)
	verifyServiceStatus(t, resp, "service1-cpu", true, map[string]int{"CPU": 2})
	verifyServiceStatus(t, resp, "service2-gpu", true, map[string]int{"GPU": 1})
	verifyTotalResourceUsage(t, resp, map[string]int{
		"CPU": 2,
		"GPU": 1,
	})

	// Activate Service 3 (CPU+GPU)
	t.Log("Activating Service 3 (CPU+GPU)")
	pid3 := runReadPidCloseConnection(t, "localhost:2043")
	if pid3 == 0 {
		t.Fatal("Failed to start service3-cpu-gpu")
	}

	// Wait for status to update
	time.Sleep(1000 * time.Millisecond)

	// Check status after starting Service 3
	resp = getStatusFromManagementAPI(t)
	verifyServiceStatus(t, resp, "service1-cpu", true, map[string]int{"CPU": 2})
	verifyServiceStatus(t, resp, "service2-gpu", true, map[string]int{"GPU": 1})
	verifyServiceStatus(t, resp, "service3-cpu-gpu", true, map[string]int{"CPU": 2, "GPU": 1})
	verifyTotalResourceUsage(t, resp, map[string]int{
		"CPU": 4,
		"GPU": 2,
	})

	// Wait for Service 1 to terminate due to inactivity timeout
	t.Log("Waiting for Service 1 to terminate due to timeout")
	time.Sleep(1500 * time.Millisecond)

	resp = getStatusFromManagementAPI(t)
	verifyServiceStatus(t, resp, "service1-cpu", false, nil)
	verifyServiceStatus(t, resp, "service2-gpu", true, map[string]int{"GPU": 1})
	verifyServiceStatus(t, resp, "service3-cpu-gpu", true, map[string]int{"CPU": 2, "GPU": 1})
	verifyTotalResourceUsage(t, resp, map[string]int{
		"CPU": 2,
		"GPU": 2,
	})

	// Wait for Service 2 to terminate due to inactivity timeout
	t.Log("Waiting for Service 2 to terminate due to timeout")
	time.Sleep(1500 * time.Millisecond)

	resp = getStatusFromManagementAPI(t)
	verifyServiceStatus(t, resp, "service1-cpu", false, nil)
	verifyServiceStatus(t, resp, "service2-gpu", false, nil)
	verifyServiceStatus(t, resp, "service3-cpu-gpu", true, map[string]int{"CPU": 2, "GPU": 1})
	verifyTotalResourceUsage(t, resp, map[string]int{
		"CPU": 2,
		"GPU": 1,
	})

	// Wait for Service 3 to terminate due to inactivity timeout
	t.Log("Waiting for Service 3 to terminate due to timeout")
	time.Sleep(1500 * time.Millisecond)

	resp = getStatusFromManagementAPI(t)
	verifyServiceStatus(t, resp, "service1-cpu", false, nil)
	verifyServiceStatus(t, resp, "service2-gpu", false, nil)
	verifyServiceStatus(t, resp, "service3-cpu-gpu", false, nil)
	verifyTotalResourceUsage(t, resp, map[string]int{
		"CPU": 0,
		"GPU": 0,
	})

	// Verify all services are down
	if len(resp.RunningServices) != 0 {
		t.Errorf("Expected 0 running services at the end, got %d", len(resp.RunningServices))
		for _, svc := range resp.RunningServices {
			t.Logf("Still running: %s", svc.Name)
		}
	}

	// Confirm processes are actually terminated
	if isProcessRunning(pid1) {
		t.Errorf("Service 1 (pid %d) is still running but should be terminated", pid1)
	}
	if isProcessRunning(pid2) {
		t.Errorf("Service 2 (pid %d) is still running but should be terminated", pid2)
	}
	if isProcessRunning(pid3) {
		t.Errorf("Service 3 (pid %d) is still running but should be terminated", pid3)
	}
}
