package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAutoListenPortSelection verifies automatic listen port selection end to
// end: a service configured with ListenPort "auto" gets a free port, the
// choice is persisted to the SQLite database, a restart reuses the same port,
// and a restart while the old port is occupied picks (and persists) a new one.
func TestAutoListenPortSelection(t *testing.T) {
	t.Parallel()

	repoDir, err := os.Getwd()
	require.NoError(t, err)
	workDir := t.TempDir()
	databasePath := filepath.Join(workDir, defaultAutoListenPortDatabasePath)

	testConfig := Config{
		Services: []ServiceConfig{
			{
				Name:            "auto-service",
				ListenPort:      autoListenPort,
				ProxyTargetHost: "localhost",
				ProxyTargetPort: "12160",
				Command:         "./test-server/test-server",
				Args:            "-p 12160",
				Workdir:         repoDir,
			},
		},
		ManagementApi: ManagementApi{ListenPort: "4160"},
	}
	StandardizeConfigNamesAndPaths(&testConfig, t.Name())
	serviceName := testConfig.Services[0].Name
	testConfig.Services[0].LogFilePath = filepath.Join(workDir, "auto-service.log")
	configFilePath := createTempConfig(t, testConfig)

	startProxyInstance := func() (*stopHandle, error) {
		waitChannel := make(chan error, 1)
		cmd, err := startLargeModelProxy("auto-listen-port", configFilePath, workDir, waitChannel)
		if err != nil {
			return nil, err
		}
		return &stopHandle{cmd: cmd, waitChannel: waitChannel}, nil
	}

	waitForAssignedPort := func() int {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			store, err := openListenPortStore(databasePath)
			if err == nil {
				port, found, getErr := store.getLastUsedPort(serviceName)
				_ = store.close()
				if getErr == nil && found {
					return port
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("service %s never got a port persisted in %s", serviceName, databasePath)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	verifyServiceReachableThroughPort := func(port int) {
		t.Helper()
		connection, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", port), 10*time.Second)
		if err != nil {
			t.Fatalf("failed to connect to automatically selected port %d: %v", port, err)
		}
		defer func() { _ = connection.Close() }()
		_ = connection.SetReadDeadline(time.Now().Add(15 * time.Second))
		pid := readPidFromOpenConnection(t, connection)
		assert.True(t, isProcessRunning(pid))
	}

	// First run: a port is selected and persisted
	proxy, err := startProxyInstance()
	require.NoError(t, err)
	firstPort := waitForAssignedPort()
	verifyServiceReachableThroughPort(firstPort)
	status := getStatusFromManagementAPI(t, "localhost:4160")
	serviceStatus := findServiceInStatusResponse(status, serviceName)
	require.NotNil(t, serviceStatus)
	assert.Equal(t, fmt.Sprintf("%d", firstPort), serviceStatus.ListenPort,
		"the management API must report the resolved port")
	require.NoError(t, proxy.stop())
	waitForPortClosed(t, "localhost:4160", 15*time.Second)

	// Second run with the same database: the same port must be reused
	proxy, err = startProxyInstance()
	require.NoError(t, err)
	secondPort := waitForAssignedPort()
	assert.Equal(t, firstPort, secondPort, "a free persisted port must be reused on restart")
	verifyServiceReachableThroughPort(secondPort)
	require.NoError(t, proxy.stop())
	waitForPortClosed(t, "localhost:4160", 15*time.Second)

	// Third run with the persisted port occupied by someone else: a new port
	// must be selected and persisted instead
	blocker, err := net.Listen("tcp", fmt.Sprintf(":%d", firstPort))
	require.NoError(t, err)
	proxy, err = startProxyInstance()
	require.NoError(t, err)
	thirdPort := waitForAssignedPort()
	assert.NotEqual(t, firstPort, thirdPort, "an occupied persisted port must not be reused")
	verifyServiceReachableThroughPort(thirdPort)
	require.NoError(t, proxy.stop())
	_ = blocker.Close()
	waitForPortClosed(t, "localhost:4160", 15*time.Second)

	// The database must reflect the latest choice
	store, err := openListenPortStore(databasePath)
	require.NoError(t, err)
	port, found, getErr := store.getLastUsedPort(serviceName)
	require.NoError(t, getErr)
	require.NoError(t, store.close())
	assert.True(t, found)
	assert.Equal(t, thirdPort, port)
}

type stopHandle struct {
	cmd         *exec.Cmd
	waitChannel chan error
}

func (h *stopHandle) stop() error {
	return stopApplication(h.cmd, h.waitChannel)
}

func waitForPortClosed(t *testing.T, address string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if err := checkPortClosed(address); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("port %s did not close within %s", address, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
