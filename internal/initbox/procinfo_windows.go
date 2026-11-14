package initbox

import (
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

const processQueryLimitedInformation = 0x1000

var procGetProcessMemoryInfo = syscall.NewLazyDLL("kernel32.dll").NewProc("K32GetProcessMemoryInfo")

// processMemoryCountersEx corresponds to PROCESS_MEMORY_COUNTERS_EX.
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

// procInfo returns private memory and CPU time. Windows does not count threads here (0 = unknown).
func procInfo(pid int) (pinfo, error) {
	var pi pinfo
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return pi, fmt.Errorf("OpenProcess %d: %w", pid, err)
	}
	defer syscall.CloseHandle(h)
	var mc processMemoryCountersEx
	mc.cb = uint32(unsafe.Sizeof(mc))
	if r, _, e := procGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&mc)), uintptr(mc.cb)); r == 0 {
		return pi, fmt.Errorf("GetProcessMemoryInfo: %w", e)
	}
	pi.vmRSS, pi.private, pi.hasPrivate = int64(mc.workingSetSize), int64(mc.privateUsage), true
	var creation, exit, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(h, &creation, &exit, &kernel, &user); err == nil {
		pi.cpu = filetime(kernel) + filetime(user)
	}
	return pi, nil
}

func filetime(f syscall.Filetime) time.Duration {
	return time.Duration(int64(f.HighDateTime)<<32|int64(f.LowDateTime)) * 100
}

func hasRssAnon() bool { return true }

func signalName(*os.ProcessState) string { return "" }
