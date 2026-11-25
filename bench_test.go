package tsbox

// Benchmarks: start time, RAM and CPU per box and script, build cold and from the cache. Driver
// via TSBOX_TEST_DRIVER as for the E2E tests.
//
//	go test -run '^$' -bench . -benchtime 5x .
//	TSBOX_TEST_DRIVER=gvisor go test -run '^$' -bench . -benchtime 5x .   # Linux, root
//
// Metrics: box-ms (start until init is ready), ready-ms (until all scripts are ready), stop-ms, box-MiB
// (RAM of the whole box when idle), rss-MiB (RSS per script according to init), threads (threads per
// script), idle-mcpu (CPU of the idle box, in thousandths of a core), build-ms.

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"
)

func BenchmarkBox(b *testing.B) {
	for _, n := range []int{1, 4} {
		b.Run(fmt.Sprintf("scripts=%d", n), func(b *testing.B) { benchBox(b, n) })
	}
}

func benchBox(b *testing.B, n int) {
	e, _ := newEngine(b)
	ctx := context.Background()
	spec := BoxSpec{Name: boxFor(b), Driver: driverName()}
	if v, err := strconv.ParseFloat(os.Getenv("TSBOX_BENCH_CPU"), 64); err == nil {
		spec.Limits.CPU = v // default 0.5
	}
	if v, err := strconv.ParseFloat(os.Getenv("TSBOX_BENCH_START_CPU"), 64); err == nil {
		spec.Limits.StartCPU = v // CPU boost at start
	}
	for i := range n {
		spec.Scripts = append(spec.Scripts, ScriptSpec{Name: fmt.Sprintf("s%d", i), Entry: "testdata/scripts/service.ts"})
	}
	box, err := e.Create(ctx, spec) // build outside the measurement
	if err != nil {
		b.Fatal(err)
	}
	var boxD, readyD, stopD time.Duration
	var mem, rss, cpu, threads float64
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		t0 := time.Now()
		if err := box.Start(ctx); err != nil {
			b.Fatal(err)
		}
		boxD += time.Since(t0)
		for !allReady(box) {
			if time.Since(t0) > 2*time.Minute {
				b.Fatalf("not ready: %+v", box.Status())
			}
			time.Sleep(5 * time.Millisecond)
		}
		readyD += time.Since(t0)
		if i == 0 {
			b.StopTimer()
			mem, rss, cpu, threads = idleStats(b, box, n)
			b.StartTimer()
		}
		t1 := time.Now()
		if err := box.Stop(ctx, 5*time.Second); err != nil {
			b.Fatal(err)
		}
		stopD += time.Since(t1)
	}
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 / float64(b.N) }
	b.ReportMetric(ms(boxD), "box-ms")
	b.ReportMetric(ms(readyD), "ready-ms")
	b.ReportMetric(ms(stopD), "stop-ms")
	b.ReportMetric(mem, "box-MiB")
	b.ReportMetric(rss, "rss-MiB")
	b.ReportMetric(cpu, "idle-mcpu")
	b.ReportMetric(threads, "threads")
	b.ReportMetric(0, "ns/op") // times are in the own metrics
}

func allReady(b *Box) bool {
	for _, s := range b.Status().Scripts {
		if s.State != ScriptReady {
			return false
		}
	}
	return true
}

// idleStats measures, after 2 s of rest, the box's RAM, the mean RSS of the scripts and the CPU over 3 s.
func idleStats(b *testing.B, box *Box, n int) (mem, rss, mcpu, threads float64) {
	time.Sleep(2 * time.Second)
	box.mu.Lock()
	drv := box.drv
	box.mu.Unlock()
	s1, err := drv.Stats(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	s2, err := drv.Stats(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	st, err := box.Stats(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	for _, s := range st.Scripts {
		rss += float64(s.RSS)
		threads += float64(s.Threads)
	}
	mcpu = float64(s2.CPUUsage-s1.CPUUsage) / float64(s2.At.Sub(s1.At)) * 1000
	return float64(s2.MemoryBytes) / (1 << 20), rss / float64(n) / (1 << 20), mcpu, threads / float64(n)
}

// BenchmarkBuild: build of a script with an SDK (kv), cold (without cache) and from the build cache.
func BenchmarkBuild(b *testing.B) {
	for _, c := range []struct {
		name string
		opts []Option
	}{{"cold", []Option{WithoutBuildCache()}}, {"cached", nil}} {
		b.Run(c.name, func(b *testing.B) {
			e, _ := newEngine(b, c.opts...)
			spec := BoxSpec{Name: boxFor(b), Driver: driverName(), SDKs: []SDKUse{{Name: "kv"}},
				Scripts: []ScriptSpec{{Name: "main", Entry: "testdata/sdk/kv.ts"}}}
			if _, err := e.Check(context.Background(), spec); err != nil { // fills the cache
				b.Fatal(err)
			}
			b.ResetTimer()
			t0 := time.Now()
			for i := 0; i < b.N; i++ {
				if _, err := e.Check(context.Background(), spec); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(time.Since(t0).Microseconds())/1000/float64(b.N), "build-ms")
			b.ReportMetric(0, "ns/op")
		})
	}
}
