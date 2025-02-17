package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// isTerminal checks if the given file descriptor is attached to a terminal.
func isTerminal(fd uintptr) bool {
	var termios syscall.Termios
	_, _, err := syscall.Syscall6(
		syscall.SYS_IOCTL,
		fd,
		uintptr(syscall.TCGETS),
		uintptr(unsafe.Pointer(&termios)),
		0,
		0,
		0,
	)
	return err == 0
}

// printColoredPrefix writes an ANSI escape sequence to set the color.
func printColoredPrefix(color string) {
	fmt.Fprintf(os.Stderr, "\033[%sm", color)
}

// printColoredSuffix resets the terminal color.
func printColoredSuffix() {
	fmt.Fprint(os.Stderr, "\033[0m")
}

// FprintfError writes an error message to stderr. If stderr is a TTY,
// it prints the message in bright red.
func FprintfError(format string, args ...interface{}) {
	if isTerminal(os.Stderr.Fd()) {
		printColoredPrefix("1;31") // bright red
		fmt.Fprintf(os.Stderr, format, args...)
		printColoredSuffix()
	} else {
		// If it's not a terminal, just print normally (no colors).
		fmt.Fprintf(os.Stderr, format, args...)
	}
}
