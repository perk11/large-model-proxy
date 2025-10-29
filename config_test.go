package main

import (
	"github.com/stretchr/testify/assert"
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

func loadConfigFromString(t *testing.T, jsonStr string) (Config, error) {
	t.Helper()
	return loadConfigFromReader(strings.NewReader(jsonStr))
}

func TestValidConfigMinimal(t *testing.T) {
	t.Parallel()
	config, err := loadConfigFromString(t, `{
		"ResourcesAvailable": {
			"RAM": 10000, //Random access memory!
			"VRAM-GPU-1": 20000, /* Wow, VRAM! */
            "VRAM-GPU-2": {"Amount": 1000, "CheckCommand": "Look, I am an object!", "CheckIntervalMilliseconds": 4000},
		},
        /* A multi-line comment
           to make sure JSONc works
        */
		"OpenAiApi": { // Commenting after opening brace? Why not
			"ListenPort": "7070"
		},
		"Services": [
			{
				"Name": "serviceA",
				"ListenPort": "8080",
				"Command": "/bin/echo",
			},
			{
				"Name": "serviceB",
				"ListenPort": "8081",
				"Command": "/echo/bin"
			}
		]
	}`)
	if err != nil {
		t.Fatalf("did not expect an error but got: %v", err)
	}
	assert.Contains(t, config.ResourcesAvailable, "RAM")
	assert.Contains(t, config.ResourcesAvailable, "VRAM-GPU-1")
	assert.Contains(t, config.ResourcesAvailable, "VRAM-GPU-2")
	assert.Len(t, config.ResourcesAvailable, 3)
	assert.Equal(t, 10000, config.ResourcesAvailable["RAM"].Amount)
	assert.Equal(t, "", config.ResourcesAvailable["RAM"].CheckCommand)
	assert.Equal(t, 20000, config.ResourcesAvailable["VRAM-GPU-1"].Amount)
	assert.Equal(t, "", config.ResourcesAvailable["VRAM-GPU-1"].CheckCommand)
	assert.Equal(t, 1000, config.ResourcesAvailable["VRAM-GPU-2"].Amount)
	assert.Equal(t, "Look, I am an object!", config.ResourcesAvailable["VRAM-GPU-2"].CheckCommand)
	assert.Equal(t, uint(4000), config.ResourcesAvailable["VRAM-GPU-2"].CheckIntervalMilliseconds)
	assert.Equal(t, "7070", config.OpenAiApi.ListenPort)
	assert.Equal(t, "", config.ManagementApi.ListenPort)
	assert.Len(t, config.Services, 2)
	assert.Equal(t, "serviceA", config.Services[0].Name)
	assert.Equal(t, "8080", config.Services[0].ListenPort)
	assert.Equal(t, "/bin/echo", config.Services[0].Command)
	assert.Equal(t, "serviceB", config.Services[1].Name)
	assert.Equal(t, "8081", config.Services[1].ListenPort)
	assert.Equal(t, "/echo/bin", config.Services[1].Command)
}

func TestDuplicateServiceNames(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"ResourcesAvailable": {
			"RAM": 10000
		},
		"Services": [
			{
				"Name": "serviceX",
				"ListenPort": "8090",
				"Command": "/bin/echo",
			},
			{
				"Name": "serviceX",
				"ListenPort": "8091",
				"Command": "/bin/echo"
			}
		]
	}`)
	checkExpectedErrorMessages(t, err, []string{"duplicate service name found", "serviceX"})
}

func TestMultipleServicesSamePort(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"ResourcesAvailable": {
			"RAM": 10000
		},
		"Services": [
			{
				"Name": "service1",
				"ListenPort": "8080",
				"Command": "/bin/echo"
			},
			{
				"Name": "service2",
				"ListenPort": "8080",
				"Command": "/bin/echo"
			}
		]
	}`)
	checkExpectedErrorMessages(t, err, []string{"multiple services listening on port 8080", "service1", "service2"})
}
func TestServiceAndOpenAiAPiSamePort(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"ResourcesAvailable": {
			"RAM": 10000,
		},
	    "OpenAiApi": {
	        "ListenPort": "8080",
	    },
		"Services": [
			{
				"Name": "service1",
				"ListenPort": "8080",
				"Command": "/bin/echo",
			},
		]
	}`)
	checkExpectedErrorMessages(t, err, []string{"multiple services listening on port 8080", "Open AI API", "service1"})
}
func TestServiceAndManagementAPiSamePort(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"ResourcesAvailable": {
			"RAM": 10000,
		},
	    "ManagementApi": {
	        "ListenPort": "8080",
	    },
		"Services": [
			{
				"Name": "service1",
				"ListenPort": "8080",
				"Command": "/bin/echo",
			},
		]
	}`)
	checkExpectedErrorMessages(t, err, []string{"multiple services listening on port 8080", "service1", "Management API"})
}
func TestServiceOpenAiApiAndManagementApiSamePort(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"ResourcesAvailable": {
			"RAM": 10000,
		},
	    "ManagementApi": {
	        "ListenPort": "8080",
	    },
	    "OpenAiAPi": {
	        "ListenPort": "8080",
	    },
		"Services": [
			{
				"Name": "service1",
				"ListenPort": "7071",
				"Command": "/bin/echo",
			},
		]
	}`)
	checkExpectedErrorMessages(t, err, []string{"multiple services listening on port 8080", "Open AI API", "Management API"})
}

func TestResourceNotInResourcesAvailable(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"ResourcesAvailable": {
			"RAM": 10000
		},
		"Services": [
			{
				"Name": "serviceNeedsGPU",
				"ListenPort": "8100",
				"Command": "/bin/echo",
				"ResourceRequirements": {
					"VRAM-GPU-1": 100
				}
			}
		]
	}`)
	checkExpectedErrorMessages(t, err, []string{"requires resource \"VRAM-GPU-1\" but it is not provided"})
}

func TestHealthcheckIntervalNoCommand(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"ResourcesAvailable": {
			"RAM": 10000
		},
		"Services": [
			{
				"Name": "serviceHC",
				"ListenPort": "8110",
				"Command": "/bin/echo",
				"HealthcheckIntervalMilliseconds": 200
			}
		]
	}`)
	checkExpectedErrorMessages(t, err, []string{"has HealthcheckIntervalMilliseconds set but no HealthcheckCommand"})
}

func TestNoOpenAiApiNoListenPort(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"ResourcesAvailable": {
			"RAM": 10000,
		},
		"Services": [
			{
				"Name": "serviceOpenAI",
				"OpenAiApi": false,
				"Command": "/bin/echo",
			}
		]
	}`)
	checkExpectedErrorMessages(t, err, []string{"does not specify ListenPort", "serviceOpenAI"})
}

func TestOpenAiApiNoListenPortMultipleServices(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"ResourcesAvailable": {
			"RAM": 10000,
		},
		"Services": [
			{
				"Name": "serviceOpenAI",
				"OpenAiApi": true,
				"Command": "/bin/echo",
			},
			{
				"Name": "serviceOpenAI-2",
				"OpenAiApi": true,
				"Command": "/bin/echo",
			}
		]
	}`)
	if err != nil {
		t.Fatalf("did not expect an error but got: %v", err)
	}
}

func TestEmptyServiceName(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"ResourcesAvailable": {
			"RAM": 10000
		},
		"Services": [
			{
				"Name": "",
				"ListenPort": "8200",
				"Command": "/bin/echo"
			}
		]
	}`)
	checkExpectedErrorMessages(t, err, []string{"has an empty Name"})
}

func TestInvalidPortNumberNonNumeric(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"ResourcesAvailable": {
			"RAM": 10000
		},
		"Services": [
			{
				"Name": "badPortService",
				"ListenPort": "80abc",
				"Command": "/bin/echo"
			}
		]
	}`)
	checkExpectedErrorMessages(t, err, []string{"invalid ListenPort: \"80abc\"", "badPortService"})
}

func TestInvalidPortNumberOutOfRange(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"ResourcesAvailable": {
			"RAM": 10000
		},
		"Services": [
			{
				"Name": "bigPortService",
				"ListenPort": "99999",
				"Command": "/bin/echo"
			}
		]
	}`)
	checkExpectedErrorMessages(t, err, []string{"invalid ListenPort: \"99999\"", "bigPortService"})
}

func TestNoCommandSpecified(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"ResourcesAvailable": {
			"RAM": 10000
		},
		"Services": [
			{
				"Name": "noCommandService",
				"ListenPort": "8080"
			}
		]
	}`)
	checkExpectedErrorMessages(t, err, []string{"has no Command specified", "noCommandService"})
}

func TestStandardKillCommandWorks(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"ResourcesAvailable": {
			"RAM": 10000
		},
		"Services": [
			{
				"Name": "killCommandWorks",
				"ListenPort": "8090",
				"Command": "/bin/echo",
				"KillCommand": "/bin/echo"
			}
		]
	}`)
	if err != nil {
		t.Fatalf("did not expect an error but got: %v", err)
	}
}

func TestAllChecksPassBiggerExample(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"ResourcesAvailable": {
			"RAM": 20000,
			"VRAM-GPU-1": 10000
		},
		"OpenAiApi": {
			"ListenPort": "6060"
		},
		"ManagementApi": {
			"ListenPort": "7071"
		},
		"Services": [
			{
				"Name": "svcOk",
				"ListenPort": "9000",
				"Command": "/bin/echo",
				"ResourceRequirements": {
					"RAM": 2000
				}
			},
			{
				"Name": "svcOk2",
				"ListenPort": "9001",
				"Command": "/bin/echo",
				"KillCommand": "/bin/echo",
				"ResourceRequirements": {
					"VRAM-GPU-1": 3000
				}
			}
		]
	}`)
	if err != nil {
		t.Fatalf("did not expect an error but got: %v", err)
	}
}

func TestInvalidManagementApiPort(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"ResourcesAvailable": {
			"RAM": 10000
		},
		"ManagementApi": {
			"ListenPort": "99999"
		},
		"Services": [
			{
				"Name": "svcOk",
				"ListenPort": "9000",
				"Command": "/bin/echo"
			}
		]
	}`)
	checkExpectedErrorMessages(t, err, []string{"top-level ManagementApi.ListenPort is invalid: \"99999\""})
}

func TestNegativeShutDownAfterInactivitySeconds(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"ResourcesAvailable": {
			"RAM": 10000
		},
		"ShutDownAfterInactivitySeconds": -10,
		"Services": [
			{
				"Name": "testService",
				"ListenPort": "8080",
				"Command": "/bin/echo"
			}
		]
	}`)
	checkExpectedErrorMessages(t, err, []string{"cannot unmarshal number -10 into Go struct field Config.ShutDownAfterInactivitySeconds of type uint"})
}
func TestDefaultConsiderStoppedOnProcessExitValueIsTrue(t *testing.T) {
	t.Parallel()
	config, err := loadConfigFromString(t, `{
		"Services": [
			{
				"Name": "testService",
				"ListenPort": "8080",
				"Command": "/bin/echo"
			}
		]
	}`)
	if err != nil {
		t.Fatalf("did not expect an error but got: %v", err)
	}
	if *(config.Services[0].ConsiderStoppedOnProcessExit) != true {
		t.Fatalf("expected ConsiderStoppedOnProcessExit to be true by default")
	}
}

func TestFalseConsiderStoppedOnProcessExitValueIsParsed(t *testing.T) {
	t.Parallel()
	config, err := loadConfigFromString(t, `{
		"Services": [
			{
				"Name": "testService",
				"ListenPort": "8080",
				"Command": "/bin/echo",
				"ConsiderStoppedOnProcessExit": false,
			}
		]
	}`)
	if err != nil {
		t.Fatalf("did not expect an error but got: %v", err)
	}
	if *(config.Services[0].ConsiderStoppedOnProcessExit) != false {
		t.Fatalf("expected ConsiderStoppedOnProcessExit to be false")
	}
}
func TestTrueConsiderStoppedOnProcessExitProcessValueIsParsed(t *testing.T) {
	t.Parallel()
	config, err := loadConfigFromString(t, `{
		"Services": [
			{
				"Name": "testService",
				"ListenPort": "8080",
				"Command": "/bin/echo",
				"ConsiderStoppedOnProcessExit": true,
			}
		]
	}`)
	if err != nil {
		t.Fatalf("did not expect an error but got: %v", err)
	}
	if *(config.Services[0].ConsiderStoppedOnProcessExit) != true {
		t.Fatalf("expected ConsiderStoppedOnProcessExit to be true")
	}
}

func TestDefaultServiceUrlWorks(t *testing.T) {
	t.Parallel()
	cfg, err := loadConfigFromString(t, `{
		"DefaultServiceUrl": "http://localhost:{{.PORT}}/status",
		"ResourcesAvailable": {
			"RAM": 10000
		},
		"Services": [
			{
				"Name": "testService",
				"ListenPort": "8080",
				"Command": "/bin/echo"
			}
		]
	}`)
	if err != nil {
		t.Fatalf("did not expect an error but got: %v", err)
	}
	if cfg.DefaultServiceUrl == nil {
		t.Fatal("expected DefaultServiceUrl to be set")
	}
	if *cfg.DefaultServiceUrl != "http://localhost:{{.PORT}}/status" {
		t.Fatalf("expected DefaultServiceUrl to be 'http://localhost:{{.PORT}}/status', got %q", *cfg.DefaultServiceUrl)
	}
}

func TestServiceSpecificUrlWorks(t *testing.T) {
	t.Parallel()
	cfg, err := loadConfigFromString(t, `{
		"ResourcesAvailable": {
			"RAM": 10000
		},
		"Services": [
			{
				"Name": "testService",
				"ListenPort": "8080",
				"Command": "/bin/echo",
				"ServiceUrl": "https://custom.example.com:{{.PORT}}/health"
			}
		]
	}`)
	if err != nil {
		t.Fatalf("did not expect an error but got: %v", err)
	}
	if cfg.Services[0].ServiceUrl == nil || !cfg.Services[0].ServiceUrl.IsSet() {
		t.Fatal("expected ServiceUrl to be set")
	}
	if cfg.Services[0].ServiceUrl.Value() != "https://custom.example.com:{{.PORT}}/health" {
		t.Fatalf("expected ServiceUrl to be 'https://custom.example.com:{{.PORT}}/health', got %q", cfg.Services[0].ServiceUrl.Value())
	}
}

func TestServiceUrlExplicitlyNull(t *testing.T) {
	t.Parallel()
	cfg, err := loadConfigFromString(t, `{
		"DefaultServiceUrl": "http://localhost:{{.PORT}}/default",
		"ResourcesAvailable": {
			"RAM": 10000
		},
		"Services": [
			{
				"Name": "testService",
				"ListenPort": "8080",
				"Command": "/bin/echo",
				"ServiceUrl": null
			}
		]
	}`)
	if err != nil {
		t.Fatalf("did not expect an error but got: %v", err)
	}
	if cfg.Services[0].ServiceUrl == nil || !cfg.Services[0].ServiceUrl.IsSet() {
		t.Fatal("expected ServiceUrl to be explicitly set")
	}
	if !cfg.Services[0].ServiceUrl.IsNull() {
		t.Fatal("expected ServiceUrl to be explicitly null")
	}
}

func TestServiceUrlNotSpecifiedUsesDefault(t *testing.T) {
	t.Parallel()
	cfg, err := loadConfigFromString(t, `{
		"DefaultServiceUrl": "http://localhost:{{.PORT}}/default",
		"ResourcesAvailable": {
			"RAM": 10000
		},
		"Services": [
			{
				"Name": "testService",
				"ListenPort": "8080",
				"Command": "/bin/echo"
			}
		]
	}`)
	if err != nil {
		t.Fatalf("did not expect an error but got: %v", err)
	}
	if cfg.Services[0].ServiceUrl != nil {
		t.Fatal("expected ServiceUrl to not be set (should fall back to default)")
	}
}

func TestValidDefaultServiceUrlTemplate(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"DefaultServiceUrl": "http://localhost:{{.PORT}}/status",
		"ResourcesAvailable": {
			"RAM": 10000
		},
		"Services": [
			{
				"Name": "testService",
				"ListenPort": "8080",
				"Command": "/bin/echo"
			}
		]
	}`)
	if err != nil {
		t.Fatalf("did not expect an error for valid template but got: %v", err)
	}
}

func TestValidServiceUrlTemplate(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"ResourcesAvailable": {
			"RAM": 10000
		},
		"Services": [
			{
				"Name": "testService",
				"ListenPort": "8080",
				"Command": "/bin/echo",
				"ServiceUrl": "https://testy:{{.PORT}}/"
			}
		]
	}`)
	if err != nil {
		t.Fatalf("did not expect an error for valid template but got: %v", err)
	}
}

func TestInvalidDefaultServiceUrlTemplate(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"DefaultServiceUrl": "http://localhost:{{.PORT}/",
		"ResourcesAvailable": {
			"RAM": 10000
		},
		"Services": [
			{
				"Name": "testService",
				"ListenPort": "8080",
				"Command": "/bin/echo"
			}
		]
	}`)
	checkExpectedErrorMessages(t, err, []string{"DefaultServiceUrl contains invalid Go template"})
}

func TestInvalidServiceUrlTemplateMissingCloseBrace(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"ResourcesAvailable": {
			"RAM": 10000
		},
		"Services": [
			{
				"Name": "testService",
				"ListenPort": "8080",
				"Command": "/bin/echo",
				"ServiceUrl": "https://example.com:{{.PORT"
			}
		]
	}`)
	checkExpectedErrorMessages(t, err, []string{"service \"testService\" has invalid Go template in ServiceUrl"})
}

func TestEmptyTemplatesAreValid(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"DefaultServiceUrl": "",
		"ResourcesAvailable": {
			"RAM": 10000
		},
		"Services": [
			{
				"Name": "testService",
				"ListenPort": "8080",
				"Command": "/bin/echo",
				"ServiceUrl": ""
			}
		]
	}`)
	if err != nil {
		t.Fatalf("did not expect an error for empty templates but got: %v", err)
	}
}

func TestNullServiceUrlNotValidated(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"ResourcesAvailable": {
			"RAM": 10000
		},
		"Services": [
			{
				"Name": "testService",
				"ListenPort": "8080",
				"Command": "/bin/echo",
				"ServiceUrl": null
			}
		]
	}`)
	if err != nil {
		t.Fatalf("did not expect an error for null ServiceUrl but got: %v", err)
	}
}

func TestMultipleServicesWithInvalidTemplates(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"DefaultServiceUrl": "{{.PORT",
		"ResourcesAvailable": {
			"RAM": 10000
		},
		"Services": [
			{
				"Name": "service1",
				"ListenPort": "8080",
				"Command": "/bin/echo",
				"ServiceUrl": "{{.HOST"
			},
			{
				"Name": "service2",
				"ListenPort": "8081",
				"Command": "/bin/echo",
				"ServiceUrl": "valid://example.com:{{.PORT}}"
			}
		]
	}`)
	checkExpectedErrorMessages(t, err, []string{
		"DefaultServiceUrl contains invalid Go template",
		"service \"service1\" has invalid Go template in ServiceUrl",
	})
	// Should not mention service2 since its template is valid
	if strings.Contains(err.Error(), "service2") {
		t.Errorf("error should not mention service2 with valid template: %v", err)
	}
}
func TestConfigJsonFileIsPickedUpByDefault(t *testing.T) {
	t.Parallel()
	waitChannel := make(chan error, 1)

	cmd, err := startLargeModelProxy("default-config-file-name-json", "", "test-configs/json", waitChannel)
	if err != nil {
		t.Fatalf("Could not start application: %v", err)
	}
	defer func() {
		if err := stopApplication(cmd, waitChannel); err != nil {
			t.Errorf("Failed to stop application: %v", err)
		}
	}()
	runReadPidCloseConnection(t, "localhost:2047")
}
func TestConfigJsoncFileIsPickedUpByDefault(t *testing.T) {
	t.Parallel()
	waitChannel := make(chan error, 1)

	cmd, err := startLargeModelProxy("default-config-file-name-jsonc", "", "test-configs/jsonc", waitChannel)
	if err != nil {
		t.Fatalf("Could not start application: %v", err)
	}
	defer func() {
		if err := stopApplication(cmd, waitChannel); err != nil {
			t.Errorf("Failed to stop application: %v", err)
		}
	}()
	runReadPidCloseConnection(t, "localhost:2048")
}
func TestConfigShouldExitIfNoConfigFileSpecifiedAndDefaultDoesNotExit(t *testing.T) {
	t.Parallel()
	waitChannel := make(chan error, 1)

	cmd, err := startLargeModelProxy("default-config-file-name-no-file", "", "test-configs/", waitChannel)
	if err == nil {
		if err := stopApplication(cmd, waitChannel); err != nil {
			t.Errorf("Failed to stop application: %v", err)
		}
		t.Fatalf("Application started, error expected")
	}
	if !strings.Contains(err.Error(), "large-model-proxy exited prematurely with error") {
		t.Fatalf("Expected error to contain 'large-model-proxy exited prematurely with error', got: %v", err)
	}
}
func TestConfigShouldExitIfNonExistingFilePathIsPassed(t *testing.T) {
	t.Parallel()
	waitChannel := make(chan error, 1)

	cmd, err := startLargeModelProxy("config-should-exit-if-non-existing-file-path-is-passed", "test-configs/i-do-not-exist.jsonc", "test-configs/", waitChannel)
	if err == nil {
		if err := stopApplication(cmd, waitChannel); err != nil {
			t.Errorf("Failed to stop application: %v", err)
		}
		t.Fatalf("Application started, error expected")
	}
	if !strings.Contains(err.Error(), "large-model-proxy exited prematurely with error") {
		t.Fatalf("Expected error to contain 'large-model-proxy exited prematurely with error', got: %v", err)
	}
}
func TestConfigShouldExitIfPassedConfigHasInvalidSyntax(t *testing.T) {
	t.Parallel()
	waitChannel := make(chan error, 1)

	cmd, err := startLargeModelProxy("config-should-exit-if-passed-config-has-invalid-syntax", "test-configs/invalid.jsonc", "test-configs/", waitChannel)
	if err == nil {
		if err := stopApplication(cmd, waitChannel); err != nil {
			t.Errorf("Failed to stop application: %v", err)
		}
		t.Fatalf("Application started, error expected")
	}
	if !strings.Contains(err.Error(), "large-model-proxy exited prematurely with error") {
		t.Fatalf("Expected error to contain 'large-model-proxy exited prematurely with error', got: %v", err)
	}
}
func TestConfigShouldExitIfDefaultConfigHasInvalidSyntax(t *testing.T) {
	t.Parallel()
	waitChannel := make(chan error, 1)

	cmd, err := startLargeModelProxy("config-should-exit-if-default-config-has-invalid-Syntax", "test-configs/invalid.jsonc", "test-configs/", waitChannel)
	if err == nil {
		if err := stopApplication(cmd, waitChannel); err != nil {
			t.Errorf("Failed to stop application: %v", err)
		}
		t.Fatalf("Application started, error expected")
	}
	if !strings.Contains(err.Error(), "large-model-proxy exited prematurely with error") {
		t.Fatalf("Expected error to contain 'large-model-proxy exited prematurely with error', got: %v", err)
	}
}
func TestNegativeStartupTimeoutMilliseconds(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"ResourcesAvailable": { "RAM": 10000 },
		"Services": [
			{
				"Name": "svc",
				"ListenPort": "8080",
				"Command": "/bin/echo",
				"StartupTimeoutMilliseconds": -5
			}
		]
	}`)
	checkExpectedErrorMessages(t, err, []string{
		"cannot unmarshal number",
	})
}

func TestStartupTimeoutMillisecondsValueIsParsed(t *testing.T) {
	t.Parallel()
	cfg, err := loadConfigFromString(t, `{
		"ResourcesAvailable": { "RAM": 10000 },
		"Services": [
			{
				"Name": "svc",
				"ListenPort": "8080",
				"Command": "/bin/echo",
				"StartupTimeoutMilliseconds": 1500
			}
		]
	}`)
	if err != nil {
		t.Fatalf("did not expect an error but got: %v", err)
	}
	if *cfg.Services[0].StartupTimeoutMilliseconds != 1500 {
		t.Fatalf("expected StartupTimeoutMilliseconds to be 1500, got %d", cfg.Services[0].StartupTimeoutMilliseconds)
	}
}

func TestUnknownFieldName(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"ResourcesAvailable": { "RAM": 10000 },
		"Services": [
			{
				"Name": "svc",
				"ListenPort": "8080",
				"Command": "/bin/echo",
				"StartupTimeoutMilliseconds": 5
			}
		],
		"Fizz": "Buzz"
	}`)
	checkExpectedErrorMessages(t, err, []string{
		"unknown field \"Fizz\"",
	})
}
func TestJsonWithExtraData(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"ResourcesAvailable": { "RAM": 10000 },
		"Services": [
			{
				"Name": "svc",
				"ListenPort": "8080",
				"Command": "/bin/echo",
				"StartupTimeoutMilliseconds": 5
			}
		]
	}
	{
	"ResourcesAvailable": { "RAM": 10000 },
			"Services": [
				{
					"Name": "svc",
					"ListenPort": "8080",
					"Command": "/bin/echo",
					"StartupTimeoutMilliseconds": 5
				}
			]
	}
`)
	checkExpectedErrorMessages(t, err, []string{
		"extra data after the first JSON object",
	})
}
func TestJsonWithInvalidDataAfterObject(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"ResourcesAvailable": { "RAM": 10000 },
		"Services": [
			{
				"Name": "svc",
				"ListenPort": "8080",
				"Command": "/bin/echo",
				"StartupTimeoutMilliseconds": 5
			}
		]
	}ExtraData`)
	checkExpectedErrorMessages(t, err, []string{
		"extra data after the first JSON object",
	})
}

func TestJsonWithExtraDataInResourcesAvailable(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"ResourcesAvailable": { "RAM": {"Amount": 10, "Foo": "Bar"} },
}`)
	checkExpectedErrorMessages(t, err, []string{
		"each entry in ResourcesAvailable must be an integer or an object with at least one of the fields",
	})
}

func TestJsonWithNonIntAmount(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"ResourcesAvailable": { "RAM": {"Amount": "Bar",} },
	}`)
	checkExpectedErrorMessages(t, err, []string{
		"each entry in ResourcesAvailable must be an integer or an object with at least one of the fields",
	})
}

func TestJsonWithNoAmountOrCommand(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"ResourcesAvailable": { "RAM": {} },
	}`)
	checkExpectedErrorMessages(t, err, []string{
		"each entry in ResourcesAvailable must be an integer or an object with at least one of the fields",
	})
}

func TestJsonWithCheckCommand(t *testing.T) {
	t.Parallel()
	config, err := loadConfigFromString(t, `{
		"ResourcesAvailable": { "RAM": {"CheckCommand": "Bar",} },
	}`)
	if err != nil {
		t.Fatalf("did not expect an error but got: %v", err)
	}
	assert.Equal(t, "Bar", config.ResourcesAvailable["RAM"].CheckCommand)
	assert.Equal(t, uint(1000), config.ResourcesAvailable["RAM"].CheckIntervalMilliseconds)
}
func TestJsonWithNonIntegerInCheckInterval(t *testing.T) {
	t.Parallel()
	_, err := loadConfigFromString(t, `{
		"ResourcesAvailable": { "CheckCommand": "Bar", "CheckFrequencyMilliseconds": "string" },
	}`)
	checkExpectedErrorMessages(t, err, []string{
		"each entry in ResourcesAvailable must be an integer or an object with at least one of the fields",
	})
}
