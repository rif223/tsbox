package build

import (
	"os/exec"
	"syscall"
	"unsafe"
)

// Windows: deno bundle starts esbuild as a service (--service --ping). When builds run in parallel, these
// processes occasionally inherit pipe handles of other builds, then wait for each other's stdin and live
// forever; cmd.Wait hung because of this in the full test run (phase4.md). A job object with
// KILL_ON_JOB_CLOSE ends all descendants of deno after every build.

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
)

const (
	jobObjectExtendedLimitInformation = 9
	limitKillOnClose                  = 0x00002000
	processSetQuota                   = 0x0100
	processTerminate                  = 0x0001
)

type extendedLimit struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
	IO                      [6]uint64
	ProcessMemoryLimit      uintptr
	JobMemoryLimit          uintptr
	PeakProcessMemoryUsed   uintptr
	PeakJobMemoryUsed       uintptr
}

// runTree runs cmd and ends all processes it started afterwards.
func runTree(cmd *exec.Cmd) error {
	job, _, _ := procCreateJobObjectW.Call(0, 0)
	if job != 0 {
		defer syscall.CloseHandle(syscall.Handle(job)) // KILL_ON_JOB_CLOSE: ends esbuild & co.
		lim := extendedLimit{LimitFlags: limitKillOnClose}
		procSetInformationJobObject.Call(job, jobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&lim)), unsafe.Sizeof(lim))
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if job != 0 {
		// Before the first child process: deno starts esbuild only after loading the module graph.
		if h, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(cmd.Process.Pid)); err == nil {
			procAssignProcessToJobObject.Call(job, uintptr(h))
			syscall.CloseHandle(h)
		}
	}
	return cmd.Wait()
}
