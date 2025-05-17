package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	ShutDownAfterInactivitySeconds                                uint
	MaxTimeToWaitForServiceToCloseConnectionBeforeGivingUpSeconds *uint
	Services                                                      []ServiceConfig `json:"Services"`
	ResourcesAvailable                                            map[string]int  `json:"ResourcesAvailable"`
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
	HealthcheckIntervalMilliseconds uint
	ShutDownAfterInactivitySeconds  uint
	RestartOnConnectionFailure      bool
	OpenAiApi                       bool
	OpenAiApiModels                 []string
	ResourceRequirements            map[string]int `json:"ResourceRequirements"`
}

type OpenAiApi struct {
	ListenPort string
}
type ManagementApi struct {
	ListenPort string
}

func loadConfig(filePath string) (Config, error) {
	var config Config

	file, err := os.ReadFile(filePath)
	if err != nil {
		return config, err
	}

	decoder := json.NewDecoder(bytes.NewReader(file))
	decoder.DisallowUnknownFields()

	err = decoder.Decode(&config)
	if err != nil {
		return config, err
	}
	err = validateConfig(config)
	if err != nil {
		return config, err
	}

	return config, nil
}

func validateConfig(cfg Config) error {
	var issues []string

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
		portSet[svc.ListenPort] = append(portSet[svc.ListenPort], svc.Name)
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
