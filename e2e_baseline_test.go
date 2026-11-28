package tsbox

import (
	"context"
	"testing"
	"time"
)

// Under gVisor init measures the RSS of an empty Deno once; later starts of the same rootfs get the value
// passed (about a second faster) and still report the scripts' own RSS, not the mapped Deno binary.
func TestBaselineRSSReused(t *testing.T) {
	needGVisor(t)
	e, _ := newEngine(t, WithStateDir(t.TempDir())) // own state: the first start measures
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
