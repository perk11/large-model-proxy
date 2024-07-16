package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type ServiceConfig struct {
	ListenPort      string // ListenPort for incoming connections
	ProxyTargetHost string // Local port the service listens on
	ProxyTargetPort string // Local port the service listens on
	Command         string // Command to run when service is starting
	Args            string // Arguments for command
	LogFilePath     string // Path to the log file for this service
}

func main() {
	configFilePath := flag.String("c", "config.json", "path to config.json")
	flag.Parse()

	configs, err := loadConfig(*configFilePath)
	if err != nil {
		fmt.Println("Error loading config:", err)
		os.Exit(1)
	}

	var wg sync.WaitGroup
	for _, config := range configs {
		wg.Add(1)
		go startProxy(config, &wg)
	}
	wg.Wait()
}
func loadConfig(filePath string) ([]ServiceConfig, error) {
	file, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	var configs []ServiceConfig
	err = json.Unmarshal(file, &configs)
	if err != nil {
		return nil, err
	}
	return configs, nil
}

func connectWithWaiting(port string, timeout time.Duration) net.Conn {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("localhost", port), time.Second)
		if err == nil {
			return conn
		}
		time.Sleep(time.Millisecond * 100)
	}
	return nil
}
func startProxy(config ServiceConfig, wg *sync.WaitGroup) {
	defer wg.Done()

	listener, err := net.Listen("tcp", ":"+config.ListenPort)
	if err != nil {
		log.Fatalf("Fatal error: cannot listen on port %s: %v", config.ListenPort, err)
	}
	defer listener.Close()

	var cmd *exec.Cmd
	var lastActivity time.Time
	inactivityTimer := time.NewTimer(time.Second * 120)

	// Service management in case of inactivity
	go func() {
		<-inactivityTimer.C
		if cmd != nil && time.Since(lastActivity) >= time.Second*120 {
			if err := cmd.Process.Kill(); err != nil {
				log.Printf("Warning: failed to kill process: %v", err)
			}
			cmd = nil
			log.Printf("Service on port %s has been stopped due to inactivity.", config.ListenPort)
		}
	}()

	for {
		clientConnection, err := listener.Accept()
		if err != nil {
			log.Printf("Warning: error accepting connection: %v", err)
			continue
		}

		// Reset inactivity timer
		lastActivity = time.Now()
		inactivityTimer.Reset(time.Second * 120)

		if cmd == nil {
			log.Printf("Starting proxy: %s %s", config.Command, config.Args)

			cmd = exec.Command(config.Command, strings.Split(config.Args, " ")...)

			logFile, err := os.OpenFile(config.LogFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
			if err != nil {
				log.Printf("Error: failed to open log file %s: %v", config.LogFilePath, err)
				clientConnection.Close()
				continue
			}
			defer logFile.Close()

			cmd.Stdout = logFile
			cmd.Stderr = logFile

			if err := cmd.Start(); err != nil {
				log.Printf("Error: failed to start service %s: %v", config.Command, err)
				clientConnection.Close()
				continue
			}

			var serviceConnection = connectWithWaiting(config.ProxyTargetPort, 30*time.Second)
			if serviceConnection == nil {
				log.Printf("Failed to connect to service on port %s\n", config.ProxyTargetPort)
				cmd.Process.Kill()
				clientConnection.Close()
				continue
			}
			log.Println("Service is accepting connections on port %s", config.ListenPort)
			go forwardConnection(clientConnection, serviceConnection)
			continue
		}

		go connectAndForwardConnection(clientConnection, config.ProxyTargetPort)
	}
}
func connectAndForwardConnection(clientConn net.Conn, servicePort string) {
	serviceConn, err := net.Dial("tcp", "localhost:"+servicePort)
	if err != nil {
		log.Printf("Error: failed to connect to service on port %s: %v", servicePort, err)
		return
	}
	defer serviceConn.Close()
	forwardConnection(clientConn, serviceConn)
}
func forwardConnection(clientConn net.Conn, serviceConn net.Conn) {
	defer clientConn.Close()
	defer serviceConn.Close()
	// Relay data between client and service
	go copyAndHandleErrors(serviceConn, clientConn, "service to client")
	copyAndHandleErrors(clientConn, serviceConn, "client to service")
}

func copyAndHandleErrors(dst io.Writer, src io.Reader, direction string) {
	if _, err := io.Copy(dst, src); err != nil {
		log.Printf("Error during data transfer (%s): %v", direction, err)
	}
}
