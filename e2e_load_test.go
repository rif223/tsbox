package tsbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestLoadManyBoxes: many boxes at the same time (dev 8, gVisor 4; TSBOX_LOAD_BOXES) with two scripts each
// and 40 kv calls and log lines each, next to a noisy box under full load. All boxes end regularly, every
// line belongs to the right box and the right script, in the right order, nothing gets lost.
func TestLoadManyBoxes(t *testing.T) {
	e, rec := newEngine(t)
	n := 8
	if isolatedDriver() {
		n = 4
	}
	if v, err := strconv.Atoi(os.Getenv("TSBOX_LOAD_BOXES")); err == nil && v > 0 {
		n = v
	}
	const rounds = 40
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	noisy, err := e.Run(ctx, one(t, "attack/busy.ts", func(s *BoxSpec) {
		s.Limits.CPU = 1
		s.Scripts[0].Limits = ScriptLimits{Watchdog: -1, Timeout: 8 * time.Minute}
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer noisy.Stop(context.Background(), time.Second)

	start := time.Now()
	params := json.RawMessage(fmt.Sprintf(`{"n":%d}`, rounds))
	names := make([]string, n)
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		spec := BoxSpec{Name: boxFor(t), Storage: fmt.Sprintf("load-%d-%d-%d", os.Getpid(), time.Now().UnixNano()%1e6, i),
			SDKs: []SDKUse{{Name: "kv"}}, Scripts: []ScriptSpec{
				{Name: "a", Entry: "testdata/scripts/load.ts", Params: params},
				{Name: "b", Entry: "testdata/scripts/load.ts", Params: params},
			}}
		names[i] = spec.Name
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, err := e.Run(ctx, spec)
			if err != nil {
				errs <- fmt.Errorf("%s: %w", spec.Name, err)
				return
			}
			st, err := b.Wait(ctx)
			if err != nil {
				_ = b.Stop(context.Background(), time.Second)
				errs <- fmt.Errorf("%s does not end: %+v", spec.Name, st)
				return
			}
			for _, s := range st.Scripts {
				if s.LastExit == nil || s.LastExit.Reason != ReasonExit {
					errs <- fmt.Errorf("%s/%s: %+v", spec.Name, s.Name, s.LastExit)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		return
	}
	t.Logf("%d boxes × 2 scripts × %d kv rounds in %v (%s)", n, rounds, time.Since(start).Round(time.Millisecond), driverName())
	if st := noisy.Status(); st.State != BoxRunning {
		t.Errorf("noisy box: %s %+v", st.State, st.Scripts[0].LastExit)
	}

	// Logs are delivered asynchronously.
	want := func(box, script string) error {
		var got []string
		rec.mu.Lock()
		for _, l := range rec.logs {
			if l.Box == box && l.Script == script && strings.HasPrefix(l.Msg, "LOAD ") {
				got = append(got, l.Msg)
			}
		}
		rec.mu.Unlock()
		if len(got) != rounds+1 {
			return fmt.Errorf("%s/%s: %d lines", box, script, len(got))
		}
		for i := 1; i <= rounds; i++ {
			if got[i-1] != fmt.Sprintf("LOAD %s %d", script, i) {
				return fmt.Errorf("%s/%s: line %d = %q", box, script, i, got[i-1])
			}
		}
		if got[rounds] != fmt.Sprintf("LOAD %s done %d", script, rounds*(rounds+1)/2) {
			return fmt.Errorf("%s/%s: %q", box, script, got[rounds])
		}
		return nil
	}
	for _, box := range names {
		for _, script := range []string{"a", "b"} {
			var err error
			for i := 0; i < 60; i++ {
				if err = want(box, script); err == nil {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			if err != nil {
				t.Error(err)
			}
		}
	}
	if d := e.DroppedEvents(); d != 0 {
		t.Errorf("%d events/logs discarded", d)
	}
}
