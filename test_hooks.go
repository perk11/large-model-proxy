//go:build testhooks

package main

import (
	"log"
	"os"
	"time"
)

// The functions in this file are test-only synchronization points. Each one
// blocks while the file named by its environment variable exists, letting a test
// hold the proxy at a precise point and release it by deleting the file.
//
// They are only compiled in when the binary is built with -tags testhooks, so they
// can never run in (or be accidentally triggered from) a production build.

// waitForProcessExitHook lets TestProcessExitDuringShutdown control the timing
// between a service process exiting and monitorProcess acquiring serviceMutex
// (e.g. to reproduce a shutdown deadlock).
func waitForProcessExitHook(serviceName string) {
	waitWhileHookFileExists("PROXY_EXIT_HOOK_FILE")
}

// waitForServiceConnectHook holds a client of a running service after it has
// been let through but before the proxy connects it to the service, so it stays
// counted as waiting.
func waitForServiceConnectHook(serviceName string) {
	waitWhileHookFileExists("PROXY_CONNECT_HOOK_FILE")
}

func waitWhileHookFileExists(environmentVariable string) {
	hookFile := os.Getenv(environmentVariable)
	if hookFile == "" {
		return
	}
	if _, err := os.Stat(hookFile); err != nil {
		return
	}
	log.Printf("[General] Waiting for \"%s\" to be deleted", hookFile)
	for {
		// Break on any error (file deleted or otherwise) so a transient
		// non-IsNotExist error can never pin this goroutine in a busy-loop.
		if _, err := os.Stat(hookFile); err != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
}
