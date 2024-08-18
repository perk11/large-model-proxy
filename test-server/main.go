package main

import (
	"flag"
	"fmt"
	"net"
	"os"
)

func main() {
	port := flag.String("p", "", "Port to listen on (required)")
	flag.Parse()

	if *port == "" {
		fmt.Println("The -p parameter is required to specify the port")
		flag.Usage() // Print usage information
		os.Exit(1)
	}

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

	// Handle incoming connections
	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Println("Error accepting: ", err.Error())
			os.Exit(1)
		}
		fmt.Println("Connection received.")
		pid := os.Getpid()
		_, writeErr := conn.Write([]byte(fmt.Sprintf("%d", pid)))
		if writeErr != nil {
			fmt.Println("Error writing to connection: ", writeErr.Error())
		}

		connectionCloseErr := conn.Close()
		if connectionCloseErr != nil {
			fmt.Println("Error closing connection: ", connectionCloseErr.Error())
		}
	}
}
