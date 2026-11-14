//go:build !windows

package build

import "os/exec"

// runTree runs cmd (on Windows additionally inside a job object, see proc_windows.go).
func runTree(cmd *exec.Cmd) error { return cmd.Run() }
