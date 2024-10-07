package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"
)

var appStarted bool = false

func main() {
	port := flag.String("p", "", "Port to listen on (required)")
	healthCheckApiPort := flag.String("healthcheck-port", "", "Healthcheck API port to listen on. If not specified, healthcheck API is disabled")
	durationToSleepBeforeListening := flag.Duration("sleep-before-listening", 0, "How much time to sleep before listening starts, such as \"300ms\", \"-1.5h\" or \"2h45m\". Valid time units are \"ns\", \"us\" (or \"µs\"), \"ms\", \"s\", \"m\", \"h\". ")
	durationStartup := flag.Duration("startup-duration", 0, "How much time to sleep after listening starts but before app is responding with PID, such as \"300ms\", \"-1.5h\" or \"2h45m\". Valid time units are \"ns\", \"us\" (or \"µs\"), \"ms\", \"s\", \"m\", \"h\". ")
	durationRequestProcessing := flag.Duration("request-processing-duration", 0, "How much time to sleep after receiving a connection before responding with PID, such as \"300ms\", \"-1.5h\" or \"2h45m\". Valid time units are \"ns\", \"us\" (or \"µs\"), \"ms\", \"s\", \"m\", \"h\". ")
	durationToSleepBeforeListeningForHealthCheck := flag.Duration("sleep-before-listening-for-healthcheck", 0, "How much time to sleep before listening for healthcheck starts, such as \"300ms\", \"-1.5h\" or \"2h45m\". Valid time units are \"ns\", \"us\" (or \"µs\"), \"ms\", \"s\", \"m\", \"h\". ")
	flag.Parse()

	if *port == "" {
		fmt.Println("The -p parameter is required to specify the port")
		flag.Usage() // Print usage information
		os.Exit(1)
	}
	if *healthCheckApiPort != "" {
		go healthCheckListen(healthCheckApiPort, durationToSleepBeforeListeningForHealthCheck)
	}
	listenOnMainPort(port, durationToSleepBeforeListening, durationStartup, durationRequestProcessing)

}

type HealthcheckResponse struct {
	Message string `json:"message"`
	Status  int    `json:"status"`
}

func listenOnMainPort(port *string, sleepDuration *time.Duration, startupDuration *time.Duration, requestProcessingDuration *time.Duration) {
	time.Sleep(*sleepDuration)
	time.AfterFunc(*startupDuration, func() {
		appStarted = true
	})
	listener, err := net.Listen("tcp", ":"+*port)
	if err != nil {
		fmt.Println("Error listening:", err.Error())
		os.Exit(1)
	}
	defer func(listener net.Listener) {
		err := listener.Close()
		if err != nil {
			fmt.Println("Failed to stop listening: ", err.Error())
		}
	}(listener)
	fmt.Printf("Listening on port %s\n", *port)
	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Println("Error accepting: ", err.Error())
			os.Exit(1)
		}
		fmt.Println("Connection received.")
		var contentToWriteToSocket string
		if appStarted {
			if requestProcessingDuration.Nanoseconds() > 0 {
				fmt.Printf("Sleeping for %s before returning pid\n", requestProcessingDuration)
				time.Sleep(*requestProcessingDuration)
			}
			fmt.Println("Responding with pid")
			pid := os.Getpid()
			contentToWriteToSocket = fmt.Sprintf("%d", pid)
		} else {
			fmt.Println("Server was still starting, responding with error")
			contentToWriteToSocket = fmt.Sprintf("Error, server still starting")
		}
		_, writeErr := conn.Write([]byte(contentToWriteToSocket))
		if writeErr != nil {
			fmt.Println("Error writing to connection: ", writeErr.Error())
		}

		connectionCloseErr := conn.Close()
		if connectionCloseErr != nil {
			fmt.Println("Error closing connection: ", connectionCloseErr.Error())
		}
	}
}

func healthCheckHandler(responseWriter http.ResponseWriter, _ *http.Request) {
	var response HealthcheckResponse
	if appStarted {
		response = HealthcheckResponse{
			Message: "ok",
			Status:  200,
		}
	} else {
		response = HealthcheckResponse{
			Message: "server_starting",
			Status:  503,
		}
	}

	fmt.Println("Sending healthcheck status code:", response.Status)
	responseWriter.Header().Set("Content-Type", "application/json")
	responseWriter.WriteHeader(response.Status)

	if err := json.NewEncoder(responseWriter).Encode(response); err != nil {
		http.Error(responseWriter, "Failed to encode JSON response", http.StatusInternalServerError)
	}
}
func healthCheckListen(port *string, sleepDuration *time.Duration) {
	time.Sleep(*sleepDuration)
	http.HandleFunc("/", healthCheckHandler)
	fmt.Printf("Listening for healthcheck on port %s\n", *port)

	if err := http.ListenAndServe(":"+*port, nil); err != nil {
		log.Fatalf("Could not start healthcheck server: %s\n", err.Error())
	}
}
