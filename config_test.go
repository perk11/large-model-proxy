package main

import (
	"strings"
	"testing"
)

func checkExpectedErrorMessages(t *testing.T, err error, expectedMsgs []string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error but got nil")
	}
	errStr := err.Error()
	for _, msg := range expectedMsgs {
		if !strings.Contains(errStr, msg) {
			t.Errorf("expected error to contain %q, but got:\n%s", msg, errStr)
		}
	}
}

func TestValidConfigMinimal(t *testing.T) {
	t.Parallel()
	cfg := Config{
		ResourcesAvailable: map[string]int{"RAM": 10000, "VRAM-GPU-1": 20000},
		OpenAiApi: OpenAiApi{
			ListenPort: "7070",
		},
		Services: []ServiceConfig{
			{
				Name:       "serviceA",
				ListenPort: "8080",
				Command:    "/bin/echo",
			},
			{
				Name:       "serviceB",
				ListenPort: "8081",
				Command:    "/bin/echo",
			},
		},
	}
	err := validateConfig(cfg)
	if err != nil {
		t.Fatalf("did not expect an error but got: %v", err)
	}
}

func TestDuplicateServiceNames(t *testing.T) {
	t.Parallel()
	cfg := Config{
		ResourcesAvailable: map[string]int{"RAM": 10000},
		Services: []ServiceConfig{
			{
				Name:       "serviceX",
				ListenPort: "8090",
				Command:    "/bin/echo",
			},
			{
				Name:       "serviceX",
				ListenPort: "8091",
				Command:    "/bin/echo",
			},
		},
	}
	err := validateConfig(cfg)
	checkExpectedErrorMessages(t, err, []string{"duplicate service name found", "serviceX"})
}

func TestMultipleServicesSamePort(t *testing.T) {
	t.Parallel()
	cfg := Config{
		ResourcesAvailable: map[string]int{"RAM": 10000},
		Services: []ServiceConfig{
			{
				Name:       "service1",
				ListenPort: "8080",
				Command:    "/bin/echo",
			},
			{
				Name:       "service2",
				ListenPort: "8080",
				Command:    "/bin/echo",
			},
		},
	}
	err := validateConfig(cfg)
	checkExpectedErrorMessages(t, err, []string{"multiple services listening on port 8080", "service1", "service2"})
}

func TestResourceNotInResourcesAvailable(t *testing.T) {
	t.Parallel()
	cfg := Config{
		ResourcesAvailable: map[string]int{"RAM": 10000},
		Services: []ServiceConfig{
			{
				Name:       "serviceNeedsGPU",
				ListenPort: "8100",
				Command:    "/bin/echo",
				ResourceRequirements: map[string]int{
					"VRAM-GPU-1": 100,
				},
			},
		},
	}
	err := validateConfig(cfg)
	checkExpectedErrorMessages(t, err, []string{"requires resource \"VRAM-GPU-1\" but it is not provided"})
}

func TestHealthcheckIntervalNoCommand(t *testing.T) {
	t.Parallel()
	cfg := Config{
		ResourcesAvailable: map[string]int{"RAM": 10000},
		Services: []ServiceConfig{
			{
				Name:                            "serviceHC",
				ListenPort:                      "8110",
				Command:                         "/bin/echo",
				HealthcheckIntervalMilliseconds: 200,
			},
		},
	}
	err := validateConfig(cfg)
	checkExpectedErrorMessages(t, err, []string{"has HealthcheckIntervalMilliseconds set but no HealthcheckCommand"})
}

func TestOpenAiApiNoListenPort(t *testing.T) {
	t.Parallel()
	cfg := Config{
		ResourcesAvailable: map[string]int{"RAM": 10000},
		Services: []ServiceConfig{
			{
				Name:      "serviceOpenAI",
				OpenAiApi: false,
				Command:   "/bin/echo",
			},
		},
	}
	err := validateConfig(cfg)
	checkExpectedErrorMessages(t, err, []string{"does not specify ListenPort", "serviceOpenAI"})
}

func TestEmptyServiceName(t *testing.T) {
	t.Parallel()
	cfg := Config{
		ResourcesAvailable: map[string]int{"RAM": 10000},
		Services: []ServiceConfig{
			{
				Name:       "",
				ListenPort: "8200",
				Command:    "/bin/echo",
			},
		},
	}
	err := validateConfig(cfg)
	checkExpectedErrorMessages(t, err, []string{"has an empty Name"})
}

func TestInvalidPortNumberNonNumeric(t *testing.T) {
	t.Parallel()
	cfg := Config{
		ResourcesAvailable: map[string]int{"RAM": 10000},
		Services: []ServiceConfig{
			{
				Name:       "badPortService",
				ListenPort: "80abc",
				Command:    "/bin/echo",
			},
		},
	}
	err := validateConfig(cfg)
	checkExpectedErrorMessages(t, err, []string{"invalid ListenPort: \"80abc\"", "badPortService"})
}

func TestInvalidPortNumberOutOfRange(t *testing.T) {
	t.Parallel()
	cfg := Config{
		ResourcesAvailable: map[string]int{"RAM": 10000},
		Services: []ServiceConfig{
			{
				Name:       "bigPortService",
				ListenPort: "99999",
				Command:    "/bin/echo",
			},
		},
	}
	err := validateConfig(cfg)
	checkExpectedErrorMessages(t, err, []string{"invalid ListenPort: \"99999\"", "bigPortService"})
}

func TestNoCommandSpecified(t *testing.T) {
	t.Parallel()
	cfg := Config{
		ResourcesAvailable: map[string]int{"RAM": 10000},
		Services: []ServiceConfig{
			{
				Name:       "noCommandService",
				ListenPort: "8080",
			},
		},
	}
	err := validateConfig(cfg)
	checkExpectedErrorMessages(t, err, []string{"has no Command specified", "noCommandService"})
}

func TestStandardKillCommandWorks(t *testing.T) {
	t.Parallel()
	cfg := Config{
		ResourcesAvailable: map[string]int{"RAM": 10000},
		Services: []ServiceConfig{
			{
				Name:        "killCommandWorks",
				ListenPort:  "8090",
				Command:     "/bin/echo",
				KillCommand: stringPtr("/bin/echo"),
			},
		},
	}
	err := validateConfig(cfg)
	if err != nil {
		t.Fatalf("did not expect an error but got: %v", err)
	}
}

func TestAllChecksPassBiggerExample(t *testing.T) {
	t.Parallel()
	cfg := Config{
		ResourcesAvailable: map[string]int{
			"RAM":        20000,
			"VRAM-GPU-1": 10000,
		},
		OpenAiApi: OpenAiApi{
			ListenPort: "6060",
		},
		ManagementApi: ManagementApi{
			ListenPort: "7071",
		},
		Services: []ServiceConfig{
			{
				Name:       "svcOk",
				ListenPort: "9000",
				Command:    "/bin/echo",
				ResourceRequirements: map[string]int{
					"RAM": 2000,
				},
			},
			{
				Name:        "svcOk2",
				ListenPort:  "9001",
				Command:     "/bin/echo",
				KillCommand: stringPtr("/bin/echo"),
				ResourceRequirements: map[string]int{
					"VRAM-GPU-1": 3000,
				},
			},
		},
	}
	err := validateConfig(cfg)
	if err != nil {
		t.Fatalf("did not expect an error but got: %v", err)
	}
}

func TestInvalidManagementApiPort(t *testing.T) {
	t.Parallel()
	cfg := Config{
		ResourcesAvailable: map[string]int{"RAM": 10000},
		ManagementApi: ManagementApi{
			ListenPort: "99999",
		},
		Services: []ServiceConfig{
			{
				Name:       "svcOk",
				ListenPort: "9000",
				Command:    "/bin/echo",
			},
		},
	}
	err := validateConfig(cfg)
	checkExpectedErrorMessages(t, err, []string{"top-level ManagementApi.ListenPort is invalid: \"99999\""})
}

func stringPtr(s string) *string {
	return &s
}
