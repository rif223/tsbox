package dev

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"github.com/rif223/tsbox/internal/driver"
)

// Windows: a job object limits the whole box (init and all scripts) hard: memory
// (JOB_OBJECT_LIMIT_JOB_MEMORY), CPU (CPU_RATE_CONTROL with HARD_CAP) and process count. If tsbox closes
// the job (also by crashing), Windows ends all processes in it (KILL_ON_JOB_CLOSE).

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procQueryInformationJobObjec = kernel32.NewProc("QueryInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject       = kernel32.NewProc("TerminateJobObject")
	procGetProcessMemoryInfo     = kernel32.NewProc("K32GetProcessMemoryInfo")
)

const (
	jobObjectBasicAccountingInformation = 1
	jobObjectBasicProcessIDList         = 3
	jobObjectExtendedLimitInformation   = 9
	jobObjectCPURateControlInformation  = 15

	limitActiveProcess = 0x00000008
	limitJobMemory     = 0x00000200
	limitDieOnUnhandle = 0x00000400
	limitKillOnClose   = 0x00002000

	cpuRateEnable  = 0x1
	cpuRateHardCap = 0x4

	processSetQuota       = 0x0100
	processTerminate      = 0x0001
	processQueryLimitInfo = 0x1000
)

type basicLimit struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type ioCounters struct{ a, b, c, d, e, f uint64 }

type extendedLimit struct {
	Basic                 basicLimit
	IO                    ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

type cpuRate struct {
	ControlFlags uint32
	CPURate      uint32
}

type basicAccounting struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

type processMemoryCountersEx struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
	privateUsage               uintptr
}

type platform struct {
	job syscall.Handle
	// overhead is the charged but unused private memory of init's image (imageOverhead); it is not
	// counted against the box limit.
	overhead int64
}

func prepare(*exec.Cmd) {}

func call(p *syscall.LazyProc, args ...uintptr) error {
	if r, _, e := p.Call(args...); r == 0 {
		return e
	}
	return nil
}

func attach(cmd *exec.Cmd, l driver.Limits) (*platform, error) {
	r, _, e := procCreateJobObjectW.Call(0, 0)
	if r == 0 {
		return nil, fmt.Errorf("CreateJobObject: %w", e)
	}
	job := syscall.Handle(r)
	fail := func(what string, err error) (*platform, error) {
		syscall.CloseHandle(job)
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	// Hard limit with headroom: the driver's watchdog already ends the box at MemoryBytes and reports it
	// as OOM; the job only prevents it from growing far beyond that. The job counts init's image overhead,
	// the watchdog does not.
	overhead := imageOverhead(cmd.Path)
	ext := extendedLimit{JobMemoryLimit: uintptr(l.MemoryBytes + max(l.MemoryBytes/4, 32<<20) + overhead)}
	ext.Basic.LimitFlags = limitJobMemory | limitActiveProcess | limitKillOnClose | limitDieOnUnhandle
	ext.Basic.ActiveProcessLimit = uint32(l.PIDs) // on Windows the limit counts processes, not threads
	if err := call(procSetInformationJobObject, uintptr(job), jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&ext)), unsafe.Sizeof(ext)); err != nil {
		return fail("job limits", err)
	}
	if err := setJobCPU(job, l.CPUCores); err != nil {
		return fail("CPU limit", err)
	}
	ph, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(cmd.Process.Pid))
	if err != nil {
		return fail("OpenProcess", err)
	}
	defer syscall.CloseHandle(ph)
	// init starts scripts only after the first start frame; until then it is assigned here.
	if err := call(procAssignProcessToJobObject, uintptr(job), uintptr(ph)); err != nil {
		return fail("AssignProcessToJobObject", err)
	}
	return &platform{job: job, overhead: overhead}, nil
}

func (p *platform) kill(*exec.Cmd) error {
	if p == nil || p.job == 0 {
		return nil
	}
	return call(procTerminateJobObject, uintptr(p.job), 137)
}

func (p *platform) close() {
	if p != nil && p.job != 0 {
		syscall.CloseHandle(p.job)
		p.job = 0
	}
}

func (p *platform) stats(int) (driver.Stats, error) {
	st := driver.Stats{At: time.Now()}
	if p == nil || p.job == 0 {
		return st, errors.New("dev: no job")
	}
	var acc basicAccounting
	if err := call(procQueryInformationJobObjec, uintptr(p.job), jobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&acc)), unsafe.Sizeof(acc), 0); err != nil {
		return st, fmt.Errorf("job statistics: %w", err)
	}
	st.CPUUsage = time.Duration(acc.TotalUserTime+acc.TotalKernelTime) * 100
	st.PIDs = int(acc.ActiveProcesses)
	var ext extendedLimit
	if call(procQueryInformationJobObjec, uintptr(p.job), jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&ext)), unsafe.Sizeof(ext), 0) == nil {
		st.MemoryPeak = max(int64(ext.PeakJobMemoryUsed)-p.overhead, 0)
	}
	// Current memory: sum of the private memory of all processes in the job.
	var list struct {
		Assigned, InList uint32
		IDs              [256]uintptr
	}
	if call(procQueryInformationJobObjec, uintptr(p.job), jobObjectBasicProcessIDList,
		uintptr(unsafe.Pointer(&list)), unsafe.Sizeof(list), 0) == nil {
		for _, pid := range list.IDs[:min(list.InList, uint32(len(list.IDs)))] {
			st.MemoryBytes += privateBytes(uint32(pid))
		}
		st.MemoryBytes = max(st.MemoryBytes-p.overhead, 0)
	}
	return st, nil
}

func privateBytes(pid uint32) int64 {
	h, err := syscall.OpenProcess(processQueryLimitInfo, false, pid)
	if err != nil {
		return 0
	}
	defer syscall.CloseHandle(h)
	var mc processMemoryCountersEx
	mc.cb = uint32(unsafe.Sizeof(mc))
	if r, _, _ := procGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&mc)), uintptr(mc.cb)); r == 0 {
		return 0
	}
	return int64(mc.privateUsage)
}

func minimalEnv() []string {
	env := []string{"NO_COLOR=1"}
	for _, k := range []string{"SystemRoot", "TEMP", "TMP"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// setJobCPU sets the hard CPU limit of the job (share of all cores in 1/10000).
func setJobCPU(job syscall.Handle, cores float64) error {
	rate := uint32(cores / float64(runtime.NumCPU()) * 10000)
	rate = min(max(rate, 1), 10000)
	cr := cpuRate{ControlFlags: cpuRateEnable | cpuRateHardCap, CPURate: rate}
	return call(procSetInformationJobObject, uintptr(job), jobObjectCPURateControlInformation,
		uintptr(unsafe.Pointer(&cr)), unsafe.Sizeof(cr))
}

func (p *platform) setCPU(cores float64) error {
	if p == nil || p.job == 0 {
		return nil
	}
	return setJobCPU(p.job, cores)
}
