package tsbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Two engines with different state directories (system daemon next to tests or an embedding application)
// run a box of the same name: each gets its own cgroup, so the limits stay apart and killing one box does
// not end the other.
func TestTwoEnginesSameBoxName(t *testing.T) {
	needIsolation(t)
	dir, err := os.MkdirTemp("", "tsbox-e2e-second-")
	if err != nil {
		t.Fatal(err)
	}
	ea, _ := newEngine(t)
	eb, _ := newEngine(t, WithStateDir(filepath.Join(dir, "state")))
	t.Cleanup(func() { // after the engines closed (cleanups run in reverse order)
		_ = exec.Command("umount", "-l", filepath.Join(dir, "state", "gvisor", "runsc", "null-netns")).Run()
		removeTemp(dir)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	name := boxFor(t)
	spec := func(mem int64) BoxSpec {
		return one(t, "scripts/service.ts", func(s *BoxSpec) { s.Name, s.Limits.Memory = name, mem })
	}
	a, err := ea.Run(ctx, spec(128<<20))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background(), time.Second)
	b, err := eb.Run(ctx, spec(256<<20))
	if err != nil {
		t.Fatalf("second engine, same box name: %v", err)
	}
	defer b.Stop(context.Background(), time.Second)
	if dirs, _ := filepath.Glob(filepath.Join("/sys/fs/cgroup/tsbox/*", name)); len(dirs) != 2 {
		t.Fatalf("cgroups of the box %q: %v (want one per engine)", name, dirs)
	}
	b.mu.Lock()
	drv := b.drv
	b.mu.Unlock()
	if err := drv.Kill(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if st := a.Status(); st.State != BoxRunning {
		t.Fatalf("killing the box of the other engine ended this one: %s (%s)", st.State, st.Error)
	}
}
