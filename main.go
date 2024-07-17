package main

import (
	"bytes"
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

const IDLE_SHUTDOWN_TIMEOUT_SECONDS_DEFAULT time.Duration = 120

type Config struct {
	IdleShutdownTimeoutSeconds *time.Duration  `json:"IdleShutdownTimeoutSeconds"`
	Services                   []ServiceConfig `json:"Services"`
}
type ServiceConfig struct {
	Name                       string // Human-readable name used in logs
	ListenPort                 string // ListenPort for incoming connections
	ProxyTargetHost            string // Local port the service listens on
	ProxyTargetPort            string // Local port the service listens on
	Command                    string
	Args                       string
	LogFilePath                string // Path to the log file for this service, defaults to logs/{Name}.log
	Workdir                    string // Directory in which the command will run
	IdleShutdownTimeoutSeconds *time.Duration
}

func main() {
	configFilePath := flag.String("c", "config.json", "path to config.json")
	flag.Parse()

	config, err := loadConfig(*configFilePath)
	if err != nil {
		fmt.Println("Error loading config:", err)
		os.Exit(1)
	}

	var wg sync.WaitGroup
	for _, service := range config.Services {
		wg.Add(1)
		if service.IdleShutdownTimeoutSeconds == nil {
			if config.IdleShutdownTimeoutSeconds == nil {
				service.IdleShutdownTimeoutSeconds = new(time.Duration)
				*service.IdleShutdownTimeoutSeconds = IDLE_SHUTDOWN_TIMEOUT_SECONDS_DEFAULT
			} else {
				service.IdleShutdownTimeoutSeconds = config.IdleShutdownTimeoutSeconds
			}
		}
		go startProxy(service, &wg)
	}
	wg.Wait()
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

	return config, nil
}

func connectWithWaiting(host string, port string, timeout time.Duration) net.Conn {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), time.Second)
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
		log.Fatalf("[%s] Fatal error: cannot listen on port %s: %v", config.Name, config.ListenPort, err)
	}
	defer listener.Close()

	var cmd *exec.Cmd
	var lastActivity time.Time
	var inactivityTimer *time.Timer
	if *config.IdleShutdownTimeoutSeconds > 0 {
		inactivityTimer = time.NewTimer(time.Second * *config.IdleShutdownTimeoutSeconds)
	}

	// Service management in case of inactivity
	go func() {
		<-inactivityTimer.C
		if cmd != nil && time.Since(lastActivity) >= time.Second**config.IdleShutdownTimeoutSeconds {
			if err := cmd.Process.Kill(); err != nil {
				log.Printf("[%s] Warning: failed to kill process: %v", config.Name, err)
			}
			cmd = nil
			log.Printf("[%s] Proxied service stopped due to inactivity.", config.Name)
		}
	}()

	for {
		clientConnection, err := listener.Accept()
		log.Printf("[%s] New incoming connection on port %s", config.Name, config.ListenPort)
		if err != nil {
			log.Printf("[%s] Warning: error accepting connection: %v", config.Name, err)
			continue
		}

		// Reset inactivity timer
		lastActivity = time.Now()
		if *config.IdleShutdownTimeoutSeconds > 0 {
			inactivityTimer.Reset(time.Second * *config.IdleShutdownTimeoutSeconds)
		}

		if cmd == nil {
			log.Printf("[%s] Starting service: %s %s", config.Name, config.Command, config.Args)

			cmd = exec.Command(config.Command, strings.Split(config.Args, " ")...)

			if config.Workdir != "" {
				cmd.Dir = config.Workdir
			}
			if config.LogFilePath == "" {
				config.LogFilePath = fmt.Sprintf("logs/%s", config.Name)
			}
			logFile, err := os.OpenFile(config.LogFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
			if err != nil {
				log.Printf("[%s] Error: failed to open log file %s: %v", config.Name, config.LogFilePath, err)
				clientConnection.Close()
				continue
			}
			defer logFile.Close()

			cmd.Stdout = logFile
			cmd.Stderr = logFile

			if err := cmd.Start(); err != nil {
				log.Printf("[%s] Error: failed to start %s: %v", config.Name, config.Command, err)
				clientConnection.Close()
				continue
			}

			var serviceConnection = connectWithWaiting(config.ProxyTargetHost, config.ProxyTargetPort, 30*time.Second)
			if serviceConnection == nil {
				log.Printf("[%s] Failed to connect to service on port %s\n", config.Name, config.ProxyTargetPort)
				cmd.Process.Kill()
				clientConnection.Close()
				continue
			}
			log.Printf("[%s] Connection to service established on port %s\n", config.Name, config.ProxyTargetPort)
			go forwardConnection(config.Name, clientConnection, serviceConnection)
			continue
		}

		go connectAndForwardConnection(clientConnection, config.Name, config.ProxyTargetHost, config.ProxyTargetPort)
	}
}
func connectAndForwardConnection(clientConn net.Conn, serviceName string, serviceHost string, servicePort string) {

	serviceConn, err := net.Dial("tcp", net.JoinHostPort(serviceHost, servicePort))
	if err != nil {
		log.Printf("[%s] Error: failed to connect to %s:%s: %v", serviceName, serviceHost, servicePort, err)
		return
	}
	defer serviceConn.Close()
	forwardConnection(serviceName, clientConn, serviceConn)
}
func forwardConnection(serviceName string, clientConn net.Conn, serviceConn net.Conn) {
	defer clientConn.Close()
	defer serviceConn.Close()
	// Relay data between client and service
	go copyAndHandleErrors(serviceConn, clientConn, "["+serviceName+"] (service to client)")
	copyAndHandleErrors(clientConn, serviceConn, "["+serviceName+"] (client to service)")
}

func copyAndHandleErrors(dst io.Writer, src io.Reader, logPrefix string) {
	if _, err := io.Copy(dst, src); err != nil {
		log.Printf("%s Error during data transfer: %v", logPrefix, err)
	}
}
