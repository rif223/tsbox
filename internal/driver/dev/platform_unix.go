//go:build !windows

package dev

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/rif223/tsbox/internal/driver"
)

type platform struct{}

func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // Kill hits init and all scripts
}

func attach(*exec.Cmd, driver.Limits) (*platform, error) { return &platform{}, nil }

func (*platform) kill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

func (*platform) close() {}

func minimalEnv() []string {
	env := []string{"NO_COLOR=1"}
	if v := os.Getenv("TMPDIR"); v != "" {
		env = append(env, "TMPDIR="+v)
	}
	return env
}

func (*platform) setCPU(float64) error { return nil }
