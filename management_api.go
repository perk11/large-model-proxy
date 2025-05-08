package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// handleStatus handles the /status endpoint request
func handleStatus(responseWriter http.ResponseWriter, request *http.Request, services []ServiceConfig) {
	// ServiceStatus represents the current state of a service
	type ServiceStatus struct {
		Name               string         `json:"name"`
		ListenPort         string         `json:"listen_port"`
		IsRunning          bool           `json:"is_running"`
		ActiveConnections  int            `json:"active_connections"`
		LastUsed           time.Time      `json:"last_used"`
		ResourceAllocation map[string]int `json:"resource_allocation"`
	}

	// ResourceUsage represents the current usage of a resource
	type ResourceUsage struct {
		TotalAvailable int            `json:"total_available"`
		TotalInUse     int            `json:"total_in_use"`
		UsageByService map[string]int `json:"usage_by_service"`
	}

	// StatusResponse represents the complete status response
	type StatusResponse struct {
		AllServices     []ServiceStatus          `json:"all_services"`
		RunningServices []ServiceStatus          `json:"running_services"`
		Resources       map[string]ResourceUsage `json:"resources"`
	}

	responseWriter.Header().Set("Content-Type", "application/json; charset=utf-8")

	// Lock to safely access resource manager state
	resourceManager.serviceMutex.Lock()
	defer resourceManager.serviceMutex.Unlock()

	response := StatusResponse{
		AllServices:     make([]ServiceStatus, 0, len(services)),
		RunningServices: make([]ServiceStatus, 0),
		Resources:       make(map[string]ResourceUsage),
	}

	// Initialize resource usage tracking
	for resource := range config.ResourcesAvailable {
		response.Resources[resource] = ResourceUsage{
			TotalAvailable: config.ResourcesAvailable[resource],
			TotalInUse:     resourceManager.resourcesInUse[resource],
			UsageByService: make(map[string]int),
		}
	}

	// Process all services
	for _, service := range services {
		status := ServiceStatus{
			Name:               service.Name,
			ListenPort:         service.ListenPort,
			ResourceAllocation: service.ResourceRequirements,
		}

		// Check if service is running
		if runningService, ok := resourceManager.runningServices[service.Name]; ok {
			status.IsRunning = true
			status.ActiveConnections = runningService.activeConnections
			status.LastUsed = runningService.lastUsed

			// Add to running services list
			response.RunningServices = append(response.RunningServices, status)

			// Update resource usage by service
			for resource, amount := range runningService.resourceRequirements {
				if usage, ok := response.Resources[resource]; ok {
					usage.UsageByService[service.Name] = amount
					response.Resources[resource] = usage
				}
			}
		}

		response.AllServices = append(response.AllServices, status)
	}

	// Encode and send response
	if err := json.NewEncoder(responseWriter).Encode(response); err != nil {
		http.Error(responseWriter, "{error: \"Failed to produce JSON response\"}", http.StatusInternalServerError)
		log.Printf("Failed to produce /status JSON response: %s\n", err.Error())
	}
}

// startManagementApi starts the management API on the specified port
func startManagementApi(managementAPI ManagementApi, services []ServiceConfig) {
	mux := http.NewServeMux()

	// Add status endpoint
	mux.HandleFunc("/status", func(responseWriter http.ResponseWriter, request *http.Request) {
		handleStatus(responseWriter, request, services)
	})

	server := &http.Server{
		Addr:    ":" + managementAPI.ListenPort,
		Handler: mux,
	}

	log.Printf("[Management API] Listening on port %s", managementAPI.ListenPort)
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("Could not start management API: %s\n", err.Error())
	}
}
