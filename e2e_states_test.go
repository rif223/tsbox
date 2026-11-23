package tsbox

import (
	"context"
	"testing"
	"time"
)

// TestScriptStates: a script is "running" soon after its start, however it logs (console, only through an
// SDK, or not at all), and "ready" only after ready(). Before, only console output ended "starting".
func TestScriptStates(t *testing.T) {
	e, rec := newEngine(t)
	for _, c := range []struct {
		entry string
		sdks  []SDKUse
	}{{"scripts/states/logsdk.ts", []SDKUse{{Name: "log"}}}, {"scripts/states/console.ts", nil}, {"scripts/states/silent.ts", nil}} {
		t.Run(c.entry, func(t *testing.T) {
			spec := one(t, c.entry, func(s *BoxSpec) { s.SDKs = c.sdks })
			b, err := e.Run(context.Background(), spec)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Stop(context.Background(), time.Second)
			state := func() ScriptState { return b.Status().Scripts[0].State }
			wait := func(want ScriptState, limit time.Duration) {
				t.Helper()
				for end := time.Now().Add(limit); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
					if state() == want {
						return
					}
				}
				t.Fatalf("state %s after %v, want %s", state(), limit, want)
			}
			wait(ScriptRunning, 1900*time.Millisecond) // the script calls ready() after 2 s
			wait(ScriptReady, 10*time.Second)
			n := 0
			rec.mu.Lock()
			for _, ev := range rec.events {
				if ev.Box == spec.Name && ev.Kind == EventScriptStarted {
					n++
				}
			}
			rec.mu.Unlock()
			if n != 1 {
				t.Errorf("%d script_started events, want 1", n)
			}
		})
	}
}
