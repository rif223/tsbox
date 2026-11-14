package tsbox

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLogsFollowAndRemove(t *testing.T) {
	e, _ := newEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	spec := one(t, "scripts/ticker.ts", func(s *BoxSpec) { s.Scripts[0].Params = json.RawMessage(`{"n":15}`) })
	b, err := e.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	var got []LogLine
	done := make(chan error, 1)
	go func() {
		for l, err := range b.Logs(ctx, LogOptions{Follow: true}) {
			if err != nil {
				done <- err
				return
			}
			got = append(got, l)
		}
		done <- nil
	}()
	time.Sleep(300 * time.Millisecond) // Follow before the start
	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(got) != 15 || got[0].Msg != "tick 1" || got[14].Msg != "tick 15" {
		t.Fatalf("Follow: %d lines %+v", len(got), got)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Fatal("Seq not monotonic")
		}
	}
	// From the file, also with a new engine.
	var tail []LogLine
	for l, err := range e.Logs(ctx, spec.Name, LogOptions{Tail: 2}) {
		if err != nil {
			t.Fatal(err)
		}
		tail = append(tail, l)
	}
	if len(tail) != 2 || tail[1].Msg != "tick 15" {
		t.Fatalf("Tail: %+v", tail)
	}
	dir := filepath.Join(testState, "boxes", spec.Name)
	if _, err := os.Stat(filepath.Join(dir, "logs", "main.log")); err != nil {
		t.Fatal(err)
	}
	if err := e.Remove(ctx, spec.Name, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("box directory still there: %v", err)
	}

	// Running box: Remove without force fails, with force it works.
	spec2 := one(t, "scripts/service.ts", nil)
	b2, err := e.Run(ctx, spec2)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Remove(ctx, spec2.Name, false); err == nil {
		t.Fatal("Remove of a running box without force")
	}
	if err := e.Remove(ctx, spec2.Name, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.Box(spec2.Name); ok || b2.Status().State != BoxExited {
		t.Fatalf("after Remove: %v", b2.Status().State)
	}
}
