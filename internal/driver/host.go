package driver

import (
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
)

// isolatingArches are the CPU architectures gVisor and Firecracker exist for.
var isolatingArches = []string{"amd64", "arm64"}

// CheckArch reports an error if the isolating driver name does not exist for this CPU architecture.
func CheckArch(name string) error {
	if !slices.Contains(isolatingArches, runtime.GOARCH) {
		return fmt.Errorf("%s: not available on %s/%s (only amd64 and arm64)", name, runtime.GOOS, runtime.GOARCH)
	}
	return nil
}

// osRelease is the kernel release (a variable for tests).
var osRelease = func() string {
	b, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	return strings.TrimSpace(string(b))
}

// KernelRelease is the Linux kernel release ("" elsewhere).
func KernelRelease() string { return osRelease() }

// WSL reports whether Linux runs in WSL2 (Windows Subsystem for Linux).
func WSL() bool { return strings.Contains(strings.ToLower(osRelease()), "microsoft") }

// KVMHint explains how to get KVM on this host, for error messages when /dev/kvm is missing.
func KVMHint() string {
	switch {
	case WSL() && runtime.GOARCH == "arm64":
		return "WSL2 on Windows on ARM: Windows offers no nested virtualization there, so there is no KVM"
	case WSL():
		return "WSL2: set nestedVirtualization=true in %UserProfile%\\.wslconfig, run \"wsl --shutdown\" and check that virtualization is on in the BIOS"
	default:
		return "turn on virtualization (VT-x/AMD-V) in the BIOS or the VM settings and load kvm_intel or kvm_amd"
	}
}
