//go:build !testhooks

package main

// No-op production stubs. The real implementations live in test_hooks.go and
// are only compiled in with the "testhooks" build tag (used by `make test`).

func waitForProcessExitHook(serviceName string) {}

func waitForServiceConnectHook(serviceName string) {}
