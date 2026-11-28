package tsbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// ownStateDir is a state directory of its own for a second engine. Call it before newEngine: the cleanup
// then runs after the engine closed. runsc leaves <state>/gvisor/runsc/null-netns mounted (see TestMain).
func ownStateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tsbox-e2e-own-")
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	t.Cleanup(func() {
		if runtime.GOOS == "linux" {
			_ = exec.Command("umount", "-l", filepath.Join(state, "gvisor", "runsc", "null-netns")).Run()
		}
		removeTemp(dir)
	})
	return state
}

// Under gVisor init measures the RSS of an empty Deno once; later starts of the same rootfs get the value
// passed (about a second faster) and still report the scripts' own RSS, not the mapped Deno binary.
func TestBaselineRSSReused(t *testing.T) {
	needGVisor(t)
	e, _ := newEngine(t, WithStateDir(ownStateDir(t))) // own state: the first start measures
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	start := func() (*Box, time.Duration) {
		t0 := time.Now()
		b, err := e.Run(ctx, one(t, "scripts/service.ts", nil))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = b.Stop(context.Background(), time.Second) })
		return b, time.Since(t0)
	}
	_, first := start()
	drv, err := e.driverFor("gvisor")
	if err != nil {
		t.Fatal(err)
	}
	rootfs, phash, err := e.profileRootFS(drv, "")
	if err != nil || rootfs == "" {
		t.Fatalf("profile: %v", err)
	}
	if v := e.baselineRSS(e.baselineKey("gvisor", phash)); v <= 0 {
		t.Fatal("no baseline remembered after the first start")
	}
	b, second := start()
	t.Logf("start with measuring %v, with the remembered baseline %v", first, second)
	time.Sleep(2 * time.Second) // init measures every 500 ms
	st, err := b.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rss := st.Scripts[0].RSS; rss <= 0 || rss > 64<<20 {
		t.Fatalf("RSS of an idle script with the remembered baseline: %d", rss)
	}
}
