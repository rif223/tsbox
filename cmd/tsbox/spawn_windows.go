package main

import (
	"os"
	"os/exec"
	"syscall"
)

const (
	createNewProcessGroup  = 0x00000200
	createNoWindow         = 0x08000000
	createBreakawayFromJob = 0x01000000
)

// startDetached starts the command without a visible window in its own process group (the CLI's Ctrl+C
// does not hit the daemon) and outside the CLI's job object if possible (otherwise it ends with it). Its
// own hidden console makes sure that Deno processes open no windows.
func startDetached(mk func() *exec.Cmd) (*exec.Cmd, error) {
	cmd := mk()
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | createNoWindow | createBreakawayFromJob}
	if err := cmd.Start(); err == nil {
		return cmd, nil
	}
	// Breakaway is not allowed: stay in the CLI's job.
	cmd = mk()
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | createNoWindow}
	return cmd, cmd.Start()
}

// enableANSI turns on escape sequences in the Windows console (live table of stats).
func enableANSI() {
	h := syscall.Handle(os.Stdout.Fd())
	var mode uint32
	if syscall.GetConsoleMode(h, &mode) != nil {
		return
	}
	setConsoleMode := syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleMode")
	_, _, _ = setConsoleMode.Call(uintptr(h), uintptr(mode|0x0004)) // ENABLE_VIRTUAL_TERMINAL_PROCESSING
}
