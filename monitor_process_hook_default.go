//go:build !testhooks

package main

// waitForProcessExitHook is a no-op in production builds. The real
// implementation lives in monitor_process_hook.go and is only compiled in with
// the "testhooks" build tag (used by `make test`).
func waitForProcessExitHook(serviceName string) {}
