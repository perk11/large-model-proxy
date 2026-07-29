//go:build testhooks

package main

import (
	"log"
	"os"
	"time"
)

// waitForProcessExitHook is a test-only synchronization point used by
// TestProcessExitDuringShutdown. When the PROXY_EXIT_HOOK_FILE environment
// variable is set, it blocks until that file is deleted, letting the test
// control the timing between a service process exiting and monitorProcess
// acquiring serviceMutex (e.g. to reproduce a shutdown deadlock).
//
// It is only compiled in when the binary is built with -tags testhooks, so it
// can never run in (or be accidentally triggered from) a production build.
func waitForProcessExitHook(serviceName string) {
	hookFile := os.Getenv("PROXY_EXIT_HOOK_FILE")
	if hookFile == "" {
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
