package gvisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/rif223/tsbox/internal/driver"
)

// TestProfile: invariant I8 – no writable host path and no /tmp in the box; init has exactly the three
// capabilities for the UID switch.
func TestProfile(t *testing.T) {
	spec := driver.Spec{ID: "b1", Limits: driver.Limits{MemoryBytes: 128 << 20, CPUCores: 0.5, PIDs: 64}, ArtifactsDir: "/host/a", Init: "/x"}
	b, _ := json.Marshal(ociSpec(Config{RootFS: "/rootfs", CgroupParent: "/tsbox"}, spec, "/host/init"))
	var oci struct {
		Process struct {
			User            struct{ UID int }
			Capabilities    map[string][]string
			NoNewPrivileges bool
			Env             []string
		}
		Root   struct{ Readonly bool }
		Mounts []struct {
			Destination, Type, Source string
			Options                   []string
		}
		Linux struct {
			Resources struct {
				Memory struct{ Limit, Swap int64 }
				Pids   struct{ Limit int }
			}
		}
	}
	if err := json.Unmarshal(b, &oci); err != nil {
		t.Fatal(err)
	}
	if !oci.Root.Readonly || !oci.Process.NoNewPrivileges {
		t.Fatal("rootfs not ro or no_new_privs missing")
	}
	want := []string{"CAP_SETUID", "CAP_SETGID", "CAP_KILL"}
	for set, caps := range oci.Process.Capabilities {
		if set == "inheritable" || set == "ambient" {
			if len(caps) != 0 {
				t.Errorf("%s: %v", set, caps)
			}
			continue
		}
		if !slices.Equal(caps, want) {
			t.Errorf("%s = %v, expected %v", set, caps, want)
		}
	}
	for _, m := range oci.Mounts {
		if m.Destination == "/tmp" && !slices.Contains(m.Options, "ro") {
			t.Error("/tmp must be read-only (otherwise runsc mounts a writable tmpfs)")
		}
		if m.Type == "bind" && !slices.Contains(m.Options, "ro") {
			t.Errorf("bind mount %s is writable", m.Destination)
		}
		if strings.Contains(m.Source, "sock") {
			t.Errorf("socket mount %s", m.Source)
		}
	}
	if r := oci.Linux.Resources; r.Memory.Limit != 128<<20 || r.Memory.Swap != r.Memory.Limit || r.Pids.Limit != 64 {
		t.Errorf("Limits: %+v", r)
	}
	for _, e := range oci.Process.Env {
		if !strings.HasPrefix(e, "NO_COLOR=") {
			t.Errorf("unexpected environment %q", e)
		}
	}
}

// TestProfileRootFSAndOOM: the box's profile replaces the driver's rootfs; oomScoreAdj is in the process
// (runsc applies it to sentry and gofer).
func TestProfileRootFSAndOOM(t *testing.T) {
	spec := driver.Spec{ID: "b1", Limits: driver.Limits{MemoryBytes: 128 << 20, CPUCores: 0.5, PIDs: 64}, ArtifactsDir: "/host/a",
		Init: "/x", RootFS: "/profiles/next", OOMScoreAdj: 500}
	var oci struct {
		Process struct{ OOMScoreAdj *int }
		Root    struct{ Path string }
	}
	b, _ := json.Marshal(ociSpec(Config{RootFS: "/rootfs", CgroupParent: "/tsbox"}, spec, "/host/init"))
	if err := json.Unmarshal(b, &oci); err != nil {
		t.Fatal(err)
	}
	if oci.Root.Path != "/profiles/next" || oci.Process.OOMScoreAdj == nil || *oci.Process.OOMScoreAdj != 500 {
		t.Fatalf("%s", b)
	}
	spec.RootFS = ""
	b, _ = json.Marshal(ociSpec(Config{RootFS: "/rootfs", CgroupParent: "/tsbox"}, spec, "/host/init"))
	if json.Unmarshal(b, &oci); oci.Root.Path != "/rootfs" {
		t.Fatalf("default rootfs: %s", oci.Root.Path)
	}
	if spec.OOMScoreAdj = 1001; spec.Validate() == nil {
		t.Fatal("oom_score_adj 1001 accepted")
	}
}

func TestPlatform(t *testing.T) {
	old := KVMDevice
	defer func() { KVMDevice = old }()
	KVMDevice = t.TempDir() + "/missing"
	if err := checkPlatform("kvm"); err == nil || !strings.Contains(err.Error(), "systrap") {
		t.Fatalf("kvm without device: %v", err)
	}
	if err := checkPlatform("ptrace"); err == nil {
		t.Fatal("unknown platform accepted")
	}
	if err := checkPlatform("systrap"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Platform: "kvm", RootFS: "/x", StateDir: "/y"}); err == nil {
		t.Fatal("New with kvm without device")
	}
}

// fakeRunsc writes a runsc stand-in that fails for the platforms in bad.
func fakeRunsc(t *testing.T, bad string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "runsc")
	script := "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in --platform=" + bad + ") echo 'starting sandbox: creating process: out of memory' >&2; exit 1;; esac; done\nexit 0\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// A platform counts only if a test sandbox starts; without an explicit platform systrap falls back to kvm.
func TestChoosePlatform(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("shell script as runsc")
	}
	old := KVMDevice
	defer func() { KVMDevice = old }()
	KVMDevice = filepath.Join(t.TempDir(), "kvm")
	if err := os.WriteFile(KVMDevice, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := Config{Runsc: fakeRunsc(t, "systrap"), StateDir: t.TempDir(), Platform: "systrap"}
	p, note, err := choosePlatform(cfg, true)
	if err != nil || p != "kvm" || !strings.Contains(note, "out of memory") {
		t.Fatalf("fallback: %q %q %v", p, note, err)
	}
	if _, _, err := choosePlatform(cfg, false); err == nil || !strings.Contains(err.Error(), "systrap") {
		t.Fatalf("explicit systrap: %v", err)
	}

	cfg = Config{Runsc: fakeRunsc(t, "nothing"), StateDir: t.TempDir(), Platform: "systrap"}
	if p, note, err := choosePlatform(cfg, true); err != nil || p != "systrap" || note != "" {
		t.Fatalf("systrap works: %q %q %v", p, note, err)
	}

	cfg = Config{Runsc: fakeRunsc(t, "*"), StateDir: t.TempDir(), Platform: "systrap"}
	if _, _, err := choosePlatform(cfg, true); err == nil {
		t.Fatal("no platform works, but no error")
	}
	t.Setenv("TSBOX_GVISOR_PROBE", "0")
	if p, _, err := choosePlatform(cfg, true); err != nil || p != "systrap" {
		t.Fatalf("probe off: %q %v", p, err)
	}
}
