//go:build !linux

// Package fcguest is tsbox inside a Firecracker microVM (Linux only).
package fcguest

import (
	"fmt"
	"os"
)

// InitMain is only available on Linux.
func InitMain([]string) int {
	fmt.Fprintln(os.Stderr, "tsbox __fcinit: only inside a Linux microVM")
	return 2
}

// ExecMain is only available on Linux.
func ExecMain([]string) int {
	fmt.Fprintln(os.Stderr, "tsbox __fcexec: only inside a Linux microVM")
	return 2
}
