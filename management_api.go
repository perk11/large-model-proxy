package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// handleStatus handles the /status endpoint request
func handleStatus(responseWriter http.ResponseWriter, request *http.Request, services []ServiceConfig) {
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

	// StatusResponse represents the complete status response
	type StatusResponse struct {
		Services  []ServiceStatus          `json:"services"`
		Resources map[string]ResourceUsage `json:"resources"`
	}

	if request.Method != "GET" {
		http.Error(responseWriter, "Only GET requests allowed", http.StatusMethodNotAllowed)
		return
	}

	responseWriter.Header().Set("Content-Type", "application/json; charset=utf-8")

	// Lock to safely access resource manager state
	resourceManager.serviceMutex.Lock()
	defer resourceManager.serviceMutex.Unlock()

	response := StatusResponse{
		Services:  make([]ServiceStatus, 0, len(services)),
		Resources: make(map[string]ResourceUsage),
	}

	// Initialize resource usage tracking
	for resource := range config.ResourcesAvailable {
		response.Resources[resource] = ResourceUsage{
			TotalAvailable: *resourceManager.resourcesAvailable[resource],
			TotalInUse:     resourceManager.resourcesInUse[resource],
			UsageByService: make(map[string]int),
		}
	}

	// Process all services
	for _, service := range services {
		status := ServiceStatus{
			Name:                 service.Name,
			ListenPort:           service.ListenPort,
			ResourceRequirements: service.ResourceRequirements,
		}

		// Determine service URL template to use
		urlTemplate, err := service.GetServiceUrlTemplate(config.DefaultServiceUrl)
		if err != nil {
			log.Printf("[Management API] Failed to get service URL template for service %s: %v", service.Name, err)
		}

		// Render service URL if template is available
		if urlTemplate != nil && service.ListenPort != "" {
			var buf bytes.Buffer
			data := map[string]string{"PORT": service.ListenPort}
			err := urlTemplate.Execute(&buf, data)
			if err != nil {
				log.Printf("[Management API] Failed to render service URL template for service %s: %v", service.Name, err)
			} else {
				renderedUrl := buf.String()
				status.ServiceUrl = &renderedUrl
			}
		}

		// Check if service is running
		if runningService, ok := resourceManager.runningServices[service.Name]; ok {
			status.IsRunning = true
			status.ActiveConnections = runningService.activeConnections
			status.LastUsed = runningService.lastUsed

			// Update resource usage by service
			for resource, amount := range service.ResourceRequirements {
				if usage, ok := response.Resources[resource]; ok {
					usage.UsageByService[service.Name] = amount
					response.Resources[resource] = usage
				}
			}
		}

		response.Services = append(response.Services, status)
	}

	// Encode and send response
	if err := json.NewEncoder(responseWriter).Encode(response); err != nil {
		http.Error(responseWriter, "{error: \"Failed to produce JSON response\"}", http.StatusInternalServerError)
		log.Printf("[Management API] Failed to produce /status JSON response: %s\n", err.Error())
	}
}

//go:embed management-ui/index.html
var uiIndexContents []byte

// startManagementApi starts the management API on the specified port
func startManagementApi(managementAPI ManagementApi, services []ServiceConfig) {
	mux := http.NewServeMux()

	// Add status endpoint
	mux.HandleFunc("/status", func(responseWriter http.ResponseWriter, request *http.Request) {
		handleStatus(responseWriter, request, services)
	})

	mux.HandleFunc("/", func(responseWriter http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/" {
			http.NotFound(responseWriter, request)
			return
		}
		responseWriter.Header().Set("Content-Type", "text/html; charset=utf-8")
		responseWriter.WriteHeader(http.StatusOK)
		bytesWritten, err := responseWriter.Write(uiIndexContents)
		if err != nil {
			log.Printf("[Management API] Failed to send UI index page: %s\n", err.Error())
		}
		if bytesWritten != len(uiIndexContents) {
			log.Printf("[Management API] Incomplete index page written: %s\n", err.Error())
		}
	})
	server := &http.Server{
		Addr:    ":" + managementAPI.ListenPort,
		Handler: mux,
	}

	log.Printf("[Management API] Listening on port %s", managementAPI.ListenPort)
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("[Management API] Could not start management API: %s\n", err.Error())
	}
}
