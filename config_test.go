package main

import (
	"strings"
	"testing"
	"time"
)

func TestValidateConfig(t *testing.T) {
	testCases := []struct {
		name           string
		cfg            Config
		wantErr        bool
		expectedErrMsg []string // substrings that should appear if wantErr==true
	}{
		{
			name: "Valid config (minimal)",
			cfg: Config{
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
			},
			wantErr: false,
		},
		{
			name: "Duplicate service names",
			cfg: Config{
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
			},
			wantErr:        true,
			expectedErrMsg: []string{"duplicate service name found", "serviceX"},
		},
		{
			name: "Multiple services listening on same port",
			cfg: Config{
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
			},
			wantErr:        true,
			expectedErrMsg: []string{"multiple services listening on port 8080", "service1", "service2"},
		},
		{
			name: "Resource not in ResourcesAvailable",
			cfg: Config{
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
			},
			wantErr:        true,
			expectedErrMsg: []string{"requires resource \"VRAM-GPU-1\" but it is not provided"},
		},
		{
			name: "Healthcheck interval but no HealthcheckCommand",
			cfg: Config{
				ResourcesAvailable: map[string]int{"RAM": 10000},
				Services: []ServiceConfig{
					{
						Name:                            "serviceHC",
						ListenPort:                      "8110",
						Command:                         "/bin/echo",
						HealthcheckIntervalMilliseconds: 200,
					},
				},
			},
			wantErr:        true,
			expectedErrMsg: []string{"has HealthcheckIntervalMilliseconds set but no HealthcheckCommand"},
		},
		{
			name: "OpenAiApi=false but no ListenPort",
			cfg: Config{
				ResourcesAvailable: map[string]int{"RAM": 10000},
				Services: []ServiceConfig{
					{
						Name:      "serviceOpenAI",
						OpenAiApi: false,
						Command:   "/bin/echo",
					},
				},
			},
			wantErr:        true,
			expectedErrMsg: []string{"does not specify ListenPort", "serviceOpenAI"},
		},
		{
			name: "Empty service name",
			cfg: Config{
				ResourcesAvailable: map[string]int{"RAM": 10000},
				Services: []ServiceConfig{
					{
						Name:       "",
						ListenPort: "8200",
						Command:    "/bin/echo",
					},
				},
			},
			wantErr:        true,
			expectedErrMsg: []string{"has an empty Name"},
		},
		{
			name: "Invalid port number (non-numeric)",
			cfg: Config{
				ResourcesAvailable: map[string]int{"RAM": 10000},
				Services: []ServiceConfig{
					{
						Name:       "badPortService",
						ListenPort: "80abc",
						Command:    "/bin/echo",
					},
				},
			},
			wantErr:        true,
			expectedErrMsg: []string{"invalid ListenPort: \"80abc\"", "badPortService"},
		},
		{
			name: "Invalid port number (out of range)",
			cfg: Config{
				ResourcesAvailable: map[string]int{"RAM": 10000},
				Services: []ServiceConfig{
					{
						Name:       "bigPortService",
						ListenPort: "99999",
						Command:    "/bin/echo",
					},
				},
			},
			wantErr:        true,
			expectedErrMsg: []string{"invalid ListenPort: \"99999\"", "bigPortService"},
		},
		{
			name: "No Command specified",
			cfg: Config{
				ResourcesAvailable: map[string]int{"RAM": 10000},
				Services: []ServiceConfig{
					{
						Name:       "noCommandService",
						ListenPort: "8080",
					},
				},
			},
			wantErr:        true,
			expectedErrMsg: []string{"has no Command specified", "noCommandService"},
		},
		{
			name: "Negative ShutDownAfterInactivitySeconds",
			cfg: Config{
				ResourcesAvailable: map[string]int{"RAM": 10000},
				Services: []ServiceConfig{
					{
						Name:                           "serviceNegDuration",
						ListenPort:                     "8090",
						Command:                        "/bin/echo",
						ShutDownAfterInactivitySeconds: -10 * time.Second,
					},
				},
			},
			wantErr:        true,
			expectedErrMsg: []string{"has negative ShutDownAfterInactivitySeconds", "serviceNegDuration"},
		},
		{
			name: "Standard KillCommand works",
			cfg: Config{
				ResourcesAvailable: map[string]int{"RAM": 10000},
				Services: []ServiceConfig{
					{
						Name:        "killCommandWorks",
						ListenPort:  "8090",
						Command:     "/bin/echo",
						KillCommand: stringPtr("/bin/echo"),
					},
				},
			},
			wantErr: false,
		},
		{
			name: "All checks pass (bigger example)",
			cfg: Config{
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
			},
			wantErr: false,
		},
		{
			name: "Invalid ManagementApi port",
			cfg: Config{
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
			},
			wantErr:        true,
			expectedErrMsg: []string{"top-level ManagementApi.ListenPort is invalid: \"99999\""},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateConfig(tc.cfg)

			if tc.wantErr && err == nil {
				t.Fatalf("expected error but got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("did not expect an error but got: %v", err)
			}

			if err != nil && tc.wantErr {
				errStr := err.Error()
				// Check that all expected substrings appear
				for _, msg := range tc.expectedErrMsg {
					if !strings.Contains(errStr, msg) {
						t.Errorf("expected error to contain %q, but got:\n%s", msg, errStr)
					}
				}
			}
		})
	}
}

func stringPtr(s string) *string {
	return &s
}
