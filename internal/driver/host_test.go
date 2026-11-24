package driver

import (
	"runtime"
	"strings"
	"testing"
)

func TestKVMHint(t *testing.T) {
	old := osRelease
	defer func() { osRelease = old }()

	osRelease = func() string { return "6.6.87.2-microsoft-standard-WSL2" }
	if !WSL() {
		t.Fatal("WSL2 kernel not detected")
	}
	want := "nestedVirtualization"
	if runtime.GOARCH == "arm64" {
		want = "Windows on ARM"
	}
	if h := KVMHint(); !strings.Contains(h, want) {
		t.Errorf("WSL hint: %s", h)
	}

	osRelease = func() string { return "6.8.0-49-generic" }
	if WSL() || !strings.Contains(KVMHint(), "BIOS") {
		t.Errorf("bare metal: WSL=%v, hint %s", WSL(), KVMHint())
	}
}

func TestCheckArch(t *testing.T) {
	err := CheckArch("gvisor")
	switch runtime.GOARCH {
	case "amd64", "arm64":
		if err != nil {
			t.Fatal(err)
		}
	default:
		if err == nil || !strings.Contains(err.Error(), runtime.GOARCH) {
			t.Fatalf("%s: %v", runtime.GOARCH, err)
		}
	}
}
