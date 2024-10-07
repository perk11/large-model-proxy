package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func connectOnly(test *testing.T, proxyAddress string) {
	_, err := net.Dial("tcp", proxyAddress)
	if err != nil {
		test.Error(err)
		return
	}
	//give large-model-proxy time to start the service, so that it doesn't get killed before it started it
	//which can lead to false positive passing tests
	time.Sleep(1 * time.Second)
}

func idleTimeout(test *testing.T, proxyAddress string) {
	pid := runReadPidCloseConnection(test, proxyAddress)
	if pid == 0 {
		//runReadPidCloseConnection already failed the test
		return
	}
	secondPid := runReadPidCloseConnection(test, proxyAddress)
	if secondPid != pid {
		test.Errorf("pid is different during second connection")
		return
	}

	time.Sleep(4 * time.Second)
	if isProcessRunning(pid) {
		test.Errorf("Process is still running after connection is closed and ShutDownAfterInactivitySeconds have passed")
		return
	}

	thirdPid := runReadPidCloseConnection(test, proxyAddress)
	if thirdPid == 0 {
		return
	}
	if thirdPid == pid {
		test.Errorf("pid during third connection is the same as during first connection ")
		return
	}

	time.Sleep(4 * time.Second)
	if isProcessRunning(pid) {
		test.Errorf("Process is still running after connection is closed and ShutDownAfterInactivitySeconds have passed")
	}
}
func runReadPidCloseConnection(test *testing.T, proxyAddress string) int {
	conn, err := net.Dial("tcp", proxyAddress)
	if err != nil {
		test.Error(err)
		return 0
	}

	buffer := make([]byte, 1024)
	bytesRead, err := conn.Read(buffer)
	if err != nil {
		if err != io.EOF {
			test.Error(err)
			return 0
		}
	}
	pidString := string(buffer[:bytesRead])
	if !isNumeric(pidString) {
		test.Errorf("value \"%s\" is not numeric, expected a pid", pidString)
		return 0
	}
	pidInt, err := strconv.Atoi(pidString)
	if err != nil {
		test.Error(err, pidString)
		return 0
	}
	if pidInt <= 0 {
		test.Errorf("value \"%s\" is not a valid pid", pidString)
		return 0
	}
	if !isProcessRunning(pidInt) {
		test.Errorf("process \"%s\" is not running while connection is still open", pidString)
		return 0
	}

	err = conn.Close()
	if err != nil {
		test.Error(err)
		return 0
	}

	return pidInt
}
func minimal(test *testing.T, proxyAddress string) {
	runReadPidCloseConnection(test, proxyAddress)
}

func isNumeric(s string) bool {
	for _, char := range s {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}
func isProcessRunning(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	if errors.Is(err, syscall.ESRCH) {
		return false
	}
	if errors.Is(err, syscall.EPERM) {
		return true
	}
	return false
}
func startLargeModelProxy(testCaseName string, configPath string, waitChannel chan error) (*exec.Cmd, error) {
	cmd := exec.Command("./large-model-proxy", "-c", configPath)
	logFilePath := fmt.Sprintf("logs/test_%s.log", testCaseName)
	logFile, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	} else {
		log.Printf("Failed to open log file for test %s", logFilePath)
	}
	if err := cmd.Start(); err != nil {
		waitChannel <- err
		return nil, err
	}
	go func() {
		waitChannel <- cmd.Wait()
	}()

	time.Sleep(1 * time.Second)

	select {
	case err := <-waitChannel:
		if err != nil {
			return nil, fmt.Errorf("large-model-proxy exited prematurely with error %v", err)
		} else {
			return nil, fmt.Errorf("large-model-proxy exited prematurely with success")
		}
	default:
	}

	err = cmd.Process.Signal(syscall.Signal(0))
	if err != nil {
		if err.Error() == "os: process already finished" {
			return nil, fmt.Errorf("large-model-proxy exited prematurely")
		}
		return nil, fmt.Errorf("error checking process state: %w", err)
	}

	return cmd, nil
}

func stopApplication(cmd *exec.Cmd, waitChannel chan error) error {
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		return err
	}

	select {
	case err := <-waitChannel:
		if err != nil && err.Error() != "waitid: no child processes" && err.Error() != "wait: no child processes" {
			return err
		}
		return nil
	case <-time.After(15 * time.Second):
		// Optionally kill the process if it hasn't exited
		_ = cmd.Process.Kill()
		return errors.New("large-model-proxy process did not stop within 15 seconds after receiving SIGINT")
	}
}

func checkPortClosed(address string) error {
	_, err := net.DialTimeout("tcp", address, time.Second)
	if err == nil {
		return fmt.Errorf("port %s is still open", address)
	}
	return nil
}

func TestAppScenarios(test *testing.T) {
	tests := []struct {
		Name                          string
		ConfigPath                    string
		AddressesToCheckAfterStopping []string
		TestFunc                      func(t *testing.T)
	}{
		{
			Name:                          "minimal",
			ConfigPath:                    "test-server/minimal.json",
			AddressesToCheckAfterStopping: []string{"localhost:2000"},
			TestFunc: func(t *testing.T) {
				minimal(t, "localhost:2000")
			},
		},
		{
			Name:                          "healthcheck",
			ConfigPath:                    "test-server/healthcheck.json",
			AddressesToCheckAfterStopping: []string{"localhost:2001"},
			TestFunc: func(t *testing.T) {
				minimal(t, "localhost:2001")
			},
		},
		{
			Name:                          "healthcheck-immediate-listen-start",
			ConfigPath:                    "test-server/healthcheck-immediate-listen-start.json",
			AddressesToCheckAfterStopping: []string{"localhost:2002"},
			TestFunc: func(t *testing.T) {
				minimal(t, "localhost:2002")
			},
		},
		{
			Name:                          "healthcheck-immediate-startup-delayed-healthcheck",
			ConfigPath:                    "test-server/healthcheck-immediate-startup-delayed-healthcheck.json",
			AddressesToCheckAfterStopping: []string{"localhost:2003"},
			TestFunc: func(t *testing.T) {
				minimal(t, "localhost:2003")
			},
		},
		{
			Name:                          "healthcheck-immediate-startup",
			ConfigPath:                    "test-server/healthcheck-immediate-startup.json",
			AddressesToCheckAfterStopping: []string{"localhost:2004"},
			TestFunc: func(t *testing.T) {
				minimal(t, "localhost:2004")
			},
		},
		{
			Name:                          "healthcheck-stuck",
			ConfigPath:                    "test-server/healthcheck-stuck.json",
			AddressesToCheckAfterStopping: []string{"localhost:2005"},
			TestFunc: func(t *testing.T) {
				connectOnly(t, "localhost:2005")
			},
		},
		{
			Name:                          "service-stuck-no-healthcheck",
			ConfigPath:                    "test-server/service-stuck-no-healthcheck.json",
			AddressesToCheckAfterStopping: []string{"localhost:2006"},
			TestFunc: func(t *testing.T) {
				connectOnly(t, "localhost:2006")
			},
		},
		{
			Name:                          "idle-timeout",
			ConfigPath:                    "test-server/idle-timeout.json",
			AddressesToCheckAfterStopping: []string{"localhost:2007"},
			TestFunc: func(t *testing.T) {
				idleTimeout(t, "localhost:2007")
			},
		},
	}

	for _, testCase := range tests {
		testCase := testCase // Capture range variable
		test.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			waitChannel := make(chan error, 1)
			cmd, err := startLargeModelProxy(testCase.Name, testCase.ConfigPath, waitChannel)
			if err != nil {
				t.Fatalf("could not start application: %v", err)
			}

			defer func() {
				if cmd == nil {
					t.Errorf("not stopping application since there was a start error: %v", err)
					return
				}
				if err := stopApplication(cmd, waitChannel); err != nil {
					t.Errorf("failed to stop application: %v", err)
				}
				for _, address := range testCase.AddressesToCheckAfterStopping {
					if err := checkPortClosed(address); err != nil {
						t.Errorf("port %s is still open after application exit: %v", address, err)
					}
				}
			}()

			testCase.TestFunc(t)
		})
	}
}
