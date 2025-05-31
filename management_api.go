package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"log"
	"net/http"
	"text/template"
	"time"
)

// renderServiceUrl renders a service URL template with the given port
func renderServiceUrl(urlTemplate, port string) (string, error) {
	if urlTemplate == "" {
		return "", nil
	}

	tmpl, err := template.New("serviceUrl").Parse(urlTemplate)
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer
	data := map[string]string{"PORT": port}
	err = tmpl.Execute(&buf, data)
	if err != nil {
		return "", err
	}

	return buf.String(), nil
}

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
		AllServices     []ServiceStatus          `json:"all_services"`
		RunningServices []ServiceStatus          `json:"running_services"`
		Resources       map[string]ResourceUsage `json:"resources"`
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
			Name:                 service.Name,
			ListenPort:           service.ListenPort,
			ResourceRequirements: service.ResourceRequirements,
		}

		// Determine service URL template to use
		var urlTemplate string
		if service.ServiceUrl != nil && service.ServiceUrl.IsSet() {
			if service.ServiceUrl.IsNull() {
				// ServiceUrl explicitly set to null - no URL desired
				urlTemplate = ""
			} else {
				// ServiceUrl set to a specific template
				urlTemplate = service.ServiceUrl.Value()
			}
		} else if config.DefaultServiceUrl != nil {
			// ServiceUrl not specified - fall back to DefaultServiceUrl
			urlTemplate = *config.DefaultServiceUrl
		}

		// Render service URL if template is available and not empty
		// Only skip rendering if ServiceUrl was explicitly set to null
		if urlTemplate != "" && service.ListenPort != "" && !(service.ServiceUrl != nil && service.ServiceUrl.IsSet() && service.ServiceUrl.IsNull()) {
			renderedUrl, err := renderServiceUrl(urlTemplate, service.ListenPort)
			if err != nil {
				log.Printf("Failed to render service URL template for service %s: %v", service.Name, err)
			} else if renderedUrl != "" {
				status.ServiceUrl = &renderedUrl
			}
		}

		// Check if service is running
		if runningService, ok := resourceManager.runningServices[service.Name]; ok {
			status.IsRunning = true
			status.ActiveConnections = runningService.activeConnections
			status.LastUsed = runningService.lastUsed

			// Add to running services list
			response.RunningServices = append(response.RunningServices, status)

			// Update resource usage by service
			for resource, amount := range service.ResourceRequirements {
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
			log.Printf("Failed to send UI index page: %s\n", err.Error())
		}
		if bytesWritten != len(uiIndexContents) {
			log.Printf("Incomplete index page written: %s\n", err.Error())
		}
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
