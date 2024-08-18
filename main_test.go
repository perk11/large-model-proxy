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

func minimal(test *testing.T, proxyAddress string) {
	conn, err := net.Dial("tcp", proxyAddress)
	if err != nil {
		test.Error(err)
		return
	}

	buffer := make([]byte, 1024)
	bytesRead, err := conn.Read(buffer)
	if err != nil {
		if err != io.EOF {
			test.Error(err)
			return
		}
	}
	pidString := string(buffer[:bytesRead])
	if !isNumeric(pidString) {
		test.Errorf("value \"%s\" is not numeric, expected a pid", pidString)
		return
	}
	pidInt, err := strconv.Atoi(pidString)
	if err != nil {
		test.Error(err)
		return
	}
	if pidInt <= 0 {
		test.Errorf("value \"%s\" is not a valid pid", pidString)
		return
	}
	if !isProcessRunning(pidInt) {
		test.Errorf("process \"%s\" is not running while connection is still open", pidString)
		return
	}

	err = conn.Close()
	if err != nil {
		test.Error(err)
		return
	}

	time.Sleep(5 * time.Second)
	if !isProcessRunning(pidInt) {
		test.Errorf("process \"%s\" is still running 5 seconds after connection is closed", pidString)
	}
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
func startLargeModelProxy(testCaseName string, configPath string) (*exec.Cmd, error) {
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
		return nil, err
	}
	time.Sleep(1 * time.Second)
	return cmd, nil
}

func stopApplication(cmd *exec.Cmd) error {
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		return err
	}
	return cmd.Wait()
}

func checkPortClosed(port string) error {
	_, err := net.DialTimeout("tcp", net.JoinHostPort("localhost", port), time.Second)
	if err == nil {
		return fmt.Errorf("port %s is still open", port)
	}
	return nil
}

func TestAppScenarios(test *testing.T) {

	tests := []struct {
		Name       string
		ConfigPath string
		Port       string
		TestFunc   func(t *testing.T, proxyAddress string)
	}{
		{"minimal", "test-server/minimal.json", "2000", minimal},
	}

	for _, testCase := range tests {
		test.Run(testCase.Name, func(test *testing.T) {
			test.Parallel()

			cmd, err := startLargeModelProxy(testCase.Name, testCase.ConfigPath)
			if err != nil {
				test.Fatalf("could not start application: %v", err)
			}

			defer func() {
				if err := stopApplication(cmd); err != nil {
					test.Errorf("failed to stop application: %v", err)
				}
				if err := checkPortClosed(testCase.Port); err != nil {
					test.Errorf("port %s is still open after application exit: %v", testCase.Port, err)
				}
			}()

			proxyAddress := fmt.Sprintf("localhost:%s", testCase.Port)
			testCase.TestFunc(test, proxyAddress)
		})
	}
}
