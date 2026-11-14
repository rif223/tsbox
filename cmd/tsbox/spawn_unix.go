//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// startDetached starts the command in its own session (survives the CLI's terminal).
func startDetached(mk func() *exec.Cmd) (*exec.Cmd, error) {
	cmd := mk()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd, cmd.Start()
}

func enableANSI() {}
