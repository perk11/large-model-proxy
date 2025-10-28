package main

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// StatusResponse represents the complete status response from the management API
type StatusResponse struct {
	Services  []ServiceStatus          `json:"services"`
	Resources map[string]ResourceUsage `json:"resources"`
}

// ServiceStatus represents the current state of a service
type ServiceStatus struct {
	Name                 string         `json:"name"`
	ListenPort           string         `json:"listen_port"`
	IsRunning            bool           `json:"is_running"`
	ActiveConnections    int            `json:"active_connections"`
	LastUsed             *time.Time     `json:"last_used"`
	ServiceUrl           *string        `json:"service_url,omitempty"`
	ResourceRequirements map[string]int `json:"resource_requirements"`
}

// ResourceUsage represents the current usage of a resource
type ResourceUsage struct {
	TotalAvailable int            `json:"total_available"`
	TotalInUse     int            `json:"total_in_use"`
	UsageByService map[string]int `json:"usage_by_service"`
}

func TestManagementUI(t *testing.T) {
	t.Parallel()
	// Setup test environment
	waitChannel := make(chan error, 1)
	const testName = "management-ui-test" // Define test name for standardization

	cfg := Config{
		ShutDownAfterInactivitySeconds: 4,
		ResourcesAvailable: map[string]ResourceAvailable{
			"CPU": {Amount: 4},
		},
		ManagementApi: ManagementApi{
			ListenPort: "2044",
		},
		Services: []ServiceConfig{
			{
				Name:            "service1-cpu",
				ListenPort:      "2045",
				ProxyTargetHost: "localhost",
				ProxyTargetPort: "12045",
				Command:         "./test-server/test-server",
				Args:            "-p 12045",
				ResourceRequirements: map[string]int{
					"CPU": 2,
				},
			},
		},
	}

	StandardizeConfigNamesAndPaths(&cfg, testName, t) // Standardize names and paths
	configFilePath := createTempConfig(t, cfg)

	// Start large-model-proxy with our test configuration
	cmd, err := startLargeModelProxy(testName, configFilePath, "", waitChannel)
	if err != nil {
		t.Fatalf("Could not start application: %v", err)
	}

	defer func() {
		if err := stopApplication(cmd, waitChannel); err != nil {
			t.Errorf("Failed to stop application: %v", err)
		}

		// Verify all ports are closed after stopping
		addresses := []string{
			"localhost:2044", "localhost:2045",
			"localhost:12044", "localhost:12045",
		}
		for _, address := range addresses {
			if err := checkPortClosed(address); err != nil {
				t.Errorf("Port %s is still open after application exit", address)
			}
		}
	}()

	resp, err := http.Get("http://localhost:2044/")
	if err != nil {
		t.Fatalf("Failed to read UI homepage: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected status code 200, got %d", resp.StatusCode)
	}

	expectedContents, err := os.ReadFile("management-ui/index.html")
	if err != nil {
		t.Fatalf("Failed to read expected UI homepage: %v", err)
	}
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read UI response body: %v", err)
	}
	if !bytes.Equal(responseBody, expectedContents) {
		t.Fatalf("Unexpected UI contents: \nExpected:\n%s\nActual:\n%s", expectedContents, responseBody)
	}
}
func TestManagementAPIStatusAcrossServices(t *testing.T) {
	t.Parallel()
	// Setup test environment
	waitChannel := make(chan error, 1)
	const testName = "management-api-test" // Define test name for standardization

	cfg := Config{
		ShutDownAfterInactivitySeconds: 4,
		ResourcesAvailable: map[string]ResourceAvailable{
			"CPU": {Amount: 4},
			"GPU": {Amount: 2},
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
				ResourceRequirements: map[string]int{
					"CPU": 2,
					"GPU": 1,
				},
			},
		},
	}

	StandardizeConfigNamesAndPaths(&cfg, testName, t) // Standardize names and paths
	configFilePath := createTempConfig(t, cfg)
	const managementApiAddress = "localhost:2040"

	// Start large-model-proxy with our test configuration
	cmd, err := startLargeModelProxy(testName, configFilePath, "", waitChannel)
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
	resp := getStatusFromManagementAPI(t, managementApiAddress)

	if len(resp.Services) != 3 {
		t.Errorf("Expected 3 services in the responses, got %d", len(resp.Services))
	}
	for _, service := range resp.Services {
		verifyServiceStatus(t, resp, service.Name, false, map[string]int{
			"CPU": 0,
			"GPU": 0,
		})
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
	resp = getStatusFromManagementAPI(t, managementApiAddress)
	verifyServiceStatus(t, resp, "management-api-test_service1-cpu", true, map[string]int{"CPU": 2})
	verifyTotalResourceUsage(t, resp, map[string]int{
		"CPU": 2,
		"GPU": 0,
	})
	if len(resp.Services) != 3 {
		t.Errorf("Expected 3 services in the responses, got %d", len(resp.Services))
	}

	// Activate Service 2 (GPU)
	t.Log("Activating Service 2 (GPU)")
	pid2 := runReadPidCloseConnection(t, "localhost:2042")
	if pid2 == 0 {
		t.Fatal("Failed to start service2-gpu")
	}

	// Wait for status to update
	time.Sleep(1000 * time.Millisecond)

	// Check status after starting Service 2
	resp = getStatusFromManagementAPI(t, managementApiAddress)
	verifyServiceStatus(t, resp, "management-api-test_service1-cpu", true, map[string]int{"CPU": 2})
	verifyServiceStatus(t, resp, "management-api-test_service2-gpu", true, map[string]int{"GPU": 1})
	verifyTotalResourceUsage(t, resp, map[string]int{
		"CPU": 2,
		"GPU": 1,
	})
	if len(resp.Services) != 3 {
		t.Errorf("Expected 3 services in the responses, got %d", len(resp.Services))
	}

	// Activate Service 3 (CPU+GPU)
	t.Log("Activating Service 3 (CPU+GPU)")
	pid3 := runReadPidCloseConnection(t, "localhost:2043")
	if pid3 == 0 {
		t.Fatal("Failed to start service3-cpu-gpu")
	}

	// Wait for status to update
	time.Sleep(1000 * time.Millisecond)

	// Check status after starting Service 3
	resp = getStatusFromManagementAPI(t, managementApiAddress)
	verifyServiceStatus(t, resp, "management-api-test_service1-cpu", true, map[string]int{"CPU": 2})
	verifyServiceStatus(t, resp, "management-api-test_service2-gpu", true, map[string]int{"GPU": 1})
	verifyServiceStatus(t, resp, "management-api-test_service3-cpu-gpu", true, map[string]int{"CPU": 2, "GPU": 1})
	verifyTotalResourceUsage(t, resp, map[string]int{
		"CPU": 4,
		"GPU": 2,
	})
	if len(resp.Services) != 3 {
		t.Errorf("Expected 3 services in the responses, got %d", len(resp.Services))
	}

	// Wait for Service 1 to terminate due to inactivity timeout
	t.Log("Waiting for Service 1 to terminate due to timeout")
	time.Sleep(1250 * time.Millisecond)

	resp = getStatusFromManagementAPI(t, managementApiAddress)
	verifyServiceStatus(t, resp, "management-api-test_service1-cpu", false, nil)
	verifyServiceStatus(t, resp, "management-api-test_service2-gpu", true, map[string]int{"GPU": 1})
	verifyServiceStatus(t, resp, "management-api-test_service3-cpu-gpu", true, map[string]int{"CPU": 2, "GPU": 1})
	verifyTotalResourceUsage(t, resp, map[string]int{
		"CPU": 2,
		"GPU": 2,
	})
	if len(resp.Services) != 3 {
		t.Errorf("Expected 3 services in the responses, got %d", len(resp.Services))
	}

	// Wait for Service 2 to terminate due to inactivity timeout
	t.Log("Waiting for Service 2 to terminate due to timeout")
	time.Sleep(1250 * time.Millisecond)

	resp = getStatusFromManagementAPI(t, managementApiAddress)
	verifyServiceStatus(t, resp, "management-api-test_service1-cpu", false, nil)
	verifyServiceStatus(t, resp, "management-api-test_service2-gpu", false, nil)
	verifyServiceStatus(t, resp, "management-api-test_service3-cpu-gpu", true, map[string]int{"CPU": 2, "GPU": 1})
	verifyTotalResourceUsage(t, resp, map[string]int{
		"CPU": 2,
		"GPU": 1,
	})
	if len(resp.Services) != 3 {
		t.Errorf("Expected 3 services in the responses, got %d", len(resp.Services))
	}

	// Wait for Service 3 to terminate due to inactivity timeout
	t.Log("Waiting for Service 3 to terminate due to timeout")
	time.Sleep(1250 * time.Millisecond)

	resp = getStatusFromManagementAPI(t, managementApiAddress)
	verifyServiceStatus(t, resp, "management-api-test_service1-cpu", false, nil)
	verifyServiceStatus(t, resp, "management-api-test_service2-gpu", false, nil)
	verifyServiceStatus(t, resp, "management-api-test_service3-cpu-gpu", false, nil)
	verifyTotalResourceUsage(t, resp, map[string]int{
		"CPU": 0,
		"GPU": 0,
	})

	if len(resp.Services) != 3 {
		t.Errorf("Expected 3 services in the responses, got %d", len(resp.Services))
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

func TestManagementAPIServiceUrls(t *testing.T) {
	t.Parallel()
	// Setup test environment
	waitChannel := make(chan error, 1)
	const testName = "management-api-serviceurl-test"

	// Create a temporary config file with explicit null for ServiceUrl
	configContent := `{
		"DefaultServiceUrl": "http://localhost:{{.PORT}}/default",
		"ResourcesAvailable": {
			"CPU": 4
		},
		"ManagementApi": {
			"ListenPort": "2050"
		},
		"Services": [
			{
				"Name": "service-with-default-url",
				"ListenPort": "2051",
				"ProxyTargetHost": "localhost",
				"ProxyTargetPort": "12051",
				"Command": "./test-server/test-server",
				"Args": "-p 12051"
			},
			{
				"Name": "service-with-custom-url",
				"ListenPort": "2052",
				"ProxyTargetHost": "localhost",
				"ProxyTargetPort": "12052",
				"Command": "./test-server/test-server",
				"Args": "-p 12052",
				"ServiceUrl": "https://example.com:{{.PORT}}/custom"
			},
			{
				"Name": "service-with-no-url",
				"ListenPort": "2053",
				"ProxyTargetHost": "localhost",
				"ProxyTargetPort": "12053",
				"Command": "./test-server/test-server",
				"Args": "-p 12053",
				"ServiceUrl": null
			}
		]
	}`

	cfg, err := loadConfigFromReader(strings.NewReader(configContent))
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	StandardizeConfigNamesAndPaths(&cfg, testName, t)
	configFilePath := createTempConfig(t, cfg)

	// Start large-model-proxy
	cmd, err := startLargeModelProxy(testName, configFilePath, "", waitChannel)
	if err != nil {
		t.Fatalf("Could not start application: %v", err)
	}

	defer func() {
		if err := stopApplication(cmd, waitChannel); err != nil {
			t.Errorf("Failed to stop application: %v", err)
		}
	}()

	// Give the management API time to start
	time.Sleep(2 * time.Second)

	// Get status from management API
	resp := getStatusFromManagementAPI(t, "localhost:2050")

	// Verify service URLs are correctly rendered
	for _, service := range resp.Services {
		switch service.Name {
		case testName + "_service-with-default-url":
			expectedUrl := "http://localhost:2051/default"
			if service.ServiceUrl == nil {
				t.Errorf("Expected service %s to have ServiceUrl %q, but got nil", service.Name, expectedUrl)
			} else if *service.ServiceUrl != expectedUrl {
				t.Errorf("Expected service %s to have ServiceUrl %q, but got %q", service.Name, expectedUrl, *service.ServiceUrl)
			}

		case testName + "_service-with-custom-url":
			expectedUrl := "https://example.com:2052/custom"
			if service.ServiceUrl == nil {
				t.Errorf("Expected service %s to have ServiceUrl %q, but got nil", service.Name, expectedUrl)
			} else if *service.ServiceUrl != expectedUrl {
				t.Errorf("Expected service %s to have ServiceUrl %q, but got %q", service.Name, expectedUrl, *service.ServiceUrl)
			}

		case testName + "_service-with-no-url":
			if service.ServiceUrl != nil {
				t.Errorf("Expected service %s to have no ServiceUrl, but got %q", service.Name, *service.ServiceUrl)
			}
		}
	}
}
