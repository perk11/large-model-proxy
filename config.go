package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tidwall/jsonc"
	"io"
	"os"
	"strconv"
	"text/template"
)

// ServiceUrlOption represents an optional service URL that can distinguish between
// not set, explicitly null, and set to a value
type ServiceUrlOption struct {
	value *string
	isSet bool
}

// UnmarshalJSON implements json.Unmarshaler to handle the three states:
// 1. Field not present: isSet = false
// 2. Field explicitly null: isSet = true, value = nil
// 3. Field set to string: isSet = true, value = &string
func (s *ServiceUrlOption) UnmarshalJSON(data []byte) error {
	s.isSet = true
	if string(data) == "null" {
		s.value = nil
		return nil
	}
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return err
	}
	s.value = &str
	return nil
}

// MarshalJSON implements json.Marshaler to properly serialize the ServiceUrlOption
func (s *ServiceUrlOption) MarshalJSON() ([]byte, error) {
	if !s.isSet {
		// This should not be called for unset fields due to omitempty,
		// but if it is, treat as null
		return []byte("null"), nil
	}
	if s.value == nil {
		return []byte("null"), nil
	}
	return json.Marshal(*s.value)
}

// IsSet returns true if the field was present in JSON (even if null)
func (s *ServiceUrlOption) IsSet() bool {
	return s.isSet
}

// IsNull returns true if the field was explicitly set to null
func (s *ServiceUrlOption) IsNull() bool {
	return s.isSet && s.value == nil
}

// Value returns the string value if set, or empty string if not set/null
func (s *ServiceUrlOption) Value() string {
	if s.value == nil {
		return ""
	}
	return *s.value
}

// StringPtr returns the string pointer (nil if not set or explicitly null)
func (s *ServiceUrlOption) StringPtr() *string {
	return s.value
}

// IsEmpty returns true if the field should be omitted during JSON marshaling
func (s *ServiceUrlOption) IsEmpty() bool {
	return !s.isSet
}

type Config struct {
	ShutDownAfterInactivitySeconds                                uint
	MaxTimeToWaitForServiceToCloseConnectionBeforeGivingUpSeconds *uint
	OutputServiceLogs                                             *bool
	DefaultServiceUrl                                             *string                      `json:"DefaultServiceUrl"`
	Services                                                      []ServiceConfig              `json:"Services"`
	ResourcesAvailable                                            map[string]ResourceAvailable `json:"ResourcesAvailable"`
	OpenAiApi                                                     OpenAiApi
	ManagementApi                                                 ManagementApi
}

type ServiceConfig struct {
	Name                            string
	ListenPort                      string
	ProxyTargetHost                 string
	ProxyTargetPort                 string
	Command                         string
	Args                            string
	KillCommand                     *string
	LogFilePath                     string
	Workdir                         string
	HealthcheckCommand              string
	StartupTimeoutMilliseconds      *uint
	HealthcheckIntervalMilliseconds uint
	ShutDownAfterInactivitySeconds  uint
	RestartOnConnectionFailure      bool
	ConsiderStoppedOnProcessExit    *bool
	OpenAiApi                       bool
	OpenAiApiModels                 []string
	ServiceUrl                      *ServiceUrlOption `json:"ServiceUrl,omitempty"`
	ResourceRequirements            map[string]int    `json:"ResourceRequirements"`
}
type ResourceAvailable struct {
	Amount                    int
	CheckCommand              string
	CheckIntervalMilliseconds uint
}

// Accepts either a JSON number or an object with {Amount, CheckCommand}.
func (r *ResourceAvailable) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if bytes.Equal(trimmed, []byte("null")) {
		*r = ResourceAvailable{}
		return nil
	}

	var asInt int
	err := json.Unmarshal(trimmed, &asInt)
	if err == nil {
		*r = ResourceAvailable{Amount: asInt}
		return nil
	}

	var dto struct {
		Amount                    int    `json:"Amount"`
		CheckCommand              string `json:"CheckCommand"`
		CheckIntervalMilliseconds uint   `json:"CheckIntervalMilliseconds"`
	}

	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	err = dec.Decode(&dto)

	if err == nil && !(dto.Amount == 0 && dto.CheckCommand == "") {
		if dto.CheckIntervalMilliseconds == 0 {
			dto.CheckIntervalMilliseconds = 1000
		}
		*r = ResourceAvailable{Amount: dto.Amount, CheckCommand: dto.CheckCommand, CheckIntervalMilliseconds: dto.CheckIntervalMilliseconds}
		return nil
	}

	if err == nil {
		err = errors.New("missing both Amount and CheckCommand fields")
	}
	return fmt.Errorf("each entry in ResourcesAvailable must be an integer or an object with at least one of the fields: \"Amount\", \"CheckCommand\", e.g. \"ResourceAvailable: {\"RAM\": {\"Amount\": 1, \"CheckCommand\": \"echo 1\"}} %v", err)
}

// UnmarshalJSON implements custom unmarshaling for ServiceConfig to handle ServiceUrl and ResourcesAvailable properly
func (sc *ServiceConfig) UnmarshalJSON(data []byte) error {
	type Alias ServiceConfig
	aux := &struct {
		ServiceUrl         json.RawMessage              `json:"ServiceUrl"`
		ResourcesAvailable map[string]ResourceAvailable `json:"ResourcesAvailable"`
		*Alias
	}{
		Alias: (*Alias)(sc),
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&aux); err != nil {
		return err
	}

	// Handle ServiceUrl field
	if aux.ServiceUrl != nil {
		// Field is present in JSON
		serviceUrl := &ServiceUrlOption{}
		if err := serviceUrl.UnmarshalJSON(aux.ServiceUrl); err != nil {
			return err
		}
		sc.ServiceUrl = serviceUrl
	}
	// If aux.ServiceUrl is nil, the field was not present, so sc.ServiceUrl remains nil

	return nil
}

// GetServiceUrlTemplate returns the appropriate compiled service URL template for this service.
// It handles the logic of checking ServiceUrl state and falling back to defaultUrl.
// Returns nil template if ServiceUrl is explicitly set to null or no template is available.
func (sc *ServiceConfig) GetServiceUrlTemplate(defaultUrl *string) (*template.Template, error) {
	var templateStr string

	if sc.ServiceUrl != nil && sc.ServiceUrl.IsSet() {
		if sc.ServiceUrl.IsNull() {
			// ServiceUrl explicitly set to null - no URL desired
			return nil, nil
		}
		// ServiceUrl set to a specific template
		templateStr = sc.ServiceUrl.Value()
	} else if defaultUrl != nil {
		// ServiceUrl not specified - fall back to defaultUrl
		templateStr = *defaultUrl
	} else {
		return nil, nil
	}

	tmpl, err := template.New("serviceUrl").Parse(templateStr)
	if err != nil {
		return nil, err
	}

	return tmpl, nil
}

type OpenAiApi struct {
	ListenPort string
}
type ManagementApi struct {
	ListenPort string
}

func loadConfig(filePath string) (Config, error) {
	file, err := os.ReadFile(filePath)
	if err != nil {
		return Config{}, err
	}
	return loadConfigFromReader(bytes.NewReader(file))
}

func loadConfigFromReader(r io.Reader) (Config, error) {
	var config Config
	configBytes, err := io.ReadAll(r)
	if err != nil {
		return config, err
	}

	jsonConfigBytes := jsonc.ToJSON(configBytes)

	decoder := json.NewDecoder(bytes.NewReader(jsonConfigBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return config, err
	}
	//due to streaming nature of the decoder we need to validate that there is no extra data after the end of the first JSON object
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return config, errors.New("extra data after the first JSON object")
	}

	err = validateConfig(config)
	if err != nil {
		return config, err
	}

	if config.OutputServiceLogs == nil {
		config.OutputServiceLogs = new(bool)
		*(config.OutputServiceLogs) = true
	}
	for i, service := range config.Services {
		if service.ConsiderStoppedOnProcessExit == nil {
			config.Services[i].ConsiderStoppedOnProcessExit = new(bool)
			*(config.Services[i].ConsiderStoppedOnProcessExit) = true
		}
	}
	return config, nil
}

func validateConfig(cfg Config) error {
	var issues []string

	// Validate DefaultServiceUrl template if present
	if cfg.DefaultServiceUrl != nil && *cfg.DefaultServiceUrl != "" {
		if err := validateGoTemplate(*cfg.DefaultServiceUrl); err != nil {
			issues = append(issues,
				fmt.Sprintf("DefaultServiceUrl contains invalid Go template: %v", err))
		}
	}

	nameSet := make(map[string]bool)
	for i, svc := range cfg.Services {
		if svc.Name == "" {
			issues = append(issues,
				fmt.Sprintf("service at index %d has an empty Name", i))
			continue
		}

		if nameSet[svc.Name] {
			issues = append(issues,
				fmt.Sprintf("duplicate service name found: %q", svc.Name))
		} else {
			nameSet[svc.Name] = true
		}
	}

	portSet := make(map[string][]string) // port -> list of service names
	for _, svc := range cfg.Services {
		if svc.ListenPort != "" {
			portSet[svc.ListenPort] = append(portSet[svc.ListenPort], svc.Name)
		}
	}
	if cfg.OpenAiApi.ListenPort != "" {
		portSet[cfg.OpenAiApi.ListenPort] = append(portSet[cfg.OpenAiApi.ListenPort], "Open AI API")
	}
	if cfg.ManagementApi.ListenPort != "" {
		portSet[cfg.ManagementApi.ListenPort] = append(portSet[cfg.ManagementApi.ListenPort], "Management API")
	}
	for p, svcs := range portSet {
		if len(svcs) > 1 {
			issues = append(issues,
				fmt.Sprintf("multiple services listening on port %s: %v", p, svcs))
		}
	}

	for i, svc := range cfg.Services {
		nameOrIndex := serviceNameOrIndex(svc.Name, i)
		if svc.ListenPort == "" {
			if svc.OpenAiApi == true {
				continue
			}
			issues = append(issues,
				fmt.Sprintf("service %s does not specify ListenPort", nameOrIndex))
			continue
		}
		portVal, err := strconv.Atoi(svc.ListenPort)
		if err != nil || portVal <= 0 || portVal > 65535 {
			issues = append(issues,
				fmt.Sprintf("service %s has invalid ListenPort: %q", nameOrIndex, svc.ListenPort))
		}
	}

	for i, svc := range cfg.Services {
		nameOrIndex := serviceNameOrIndex(svc.Name, i)
		for resourceName := range svc.ResourceRequirements {
			if _, ok := cfg.ResourcesAvailable[resourceName]; !ok {
				issues = append(issues,
					fmt.Sprintf("service %s requires resource %q but it is not provided in ResourcesAvailable",
						nameOrIndex, resourceName))
			}
		}
	}

	for i, svc := range cfg.Services {
		nameOrIndex := serviceNameOrIndex(svc.Name, i)
		if svc.HealthcheckIntervalMilliseconds > 0 && svc.HealthcheckCommand == "" {
			issues = append(issues,
				fmt.Sprintf("service %s has HealthcheckIntervalMilliseconds set but no HealthcheckCommand",
					nameOrIndex))
		}
	}

	for i, svc := range cfg.Services {
		nameOrIndex := serviceNameOrIndex(svc.Name, i)
		if !svc.OpenAiApi && svc.ListenPort == "" {
			issues = append(issues,
				fmt.Sprintf("service %s is flagged OpenAiApi=false and has empty ListenPort, it is impossible to reach",
					nameOrIndex))
		}
	}

	for i, svc := range cfg.Services {
		nameOrIndex := serviceNameOrIndex(svc.Name, i)
		if svc.Command == "" {
			issues = append(issues,
				fmt.Sprintf("service %s has no Command specified", nameOrIndex))
		}
	}

	// Validate ServiceUrl templates if present and not null
	for i, svc := range cfg.Services {
		nameOrIndex := serviceNameOrIndex(svc.Name, i)
		_, err := svc.GetServiceUrlTemplate(cfg.DefaultServiceUrl)
		if err != nil {
			issues = append(issues,
				fmt.Sprintf("service %s has invalid Go template in ServiceUrl: %v", nameOrIndex, err))
		}
	}

	if cfg.OpenAiApi.ListenPort != "" {
		portVal, err := strconv.Atoi(cfg.OpenAiApi.ListenPort)
		if err != nil || portVal <= 0 || portVal > 65535 {
			issues = append(issues,
				fmt.Sprintf("top-level OpenAiApi.ListenPort is invalid: %q", cfg.OpenAiApi.ListenPort))
		}
	}

	if cfg.ManagementApi.ListenPort != "" {
		portVal, err := strconv.Atoi(cfg.ManagementApi.ListenPort)
		if err != nil || portVal <= 0 || portVal > 65535 {
			issues = append(issues,
				fmt.Sprintf("top-level ManagementApi.ListenPort is invalid: %q", cfg.ManagementApi.ListenPort))
		}
	}

	if len(issues) > 0 {
		return errors.New(" - " + joinStrings(issues, "\n - "))
	}
	return nil
}

// validateGoTemplate validates that the given string is a valid Go template
func validateGoTemplate(templateStr string) error {
	_, err := template.New("validation").Parse(templateStr)
	return err
}

// serviceNameOrIndex returns the service name if not empty, otherwise "at index i".
func serviceNameOrIndex(name string, i int) string {
	if name == "" {
		return fmt.Sprintf("at index %d", i)
	}
	return fmt.Sprintf("%q", name)
}

func joinStrings(ss []string, sep string) string {
	if len(ss) == 0 {
		return ""
	}
	out := ss[0]
	for _, s := range ss[1:] {
		out += sep + s
	}
	return out
}
