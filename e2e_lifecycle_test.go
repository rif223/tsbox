package tsbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Stop while the box is still starting (driver start or waiting for init's hello) ends the box: before, Stop
// found no driver to kill, returned "does not end" after 20 s, and Start set the box running afterwards.
func TestStopDuringStart(t *testing.T) {
	e, _ := newEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	b, err := e.Create(ctx, one(t, "scripts/service.ts", nil))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan error, 1)
	go func() { started <- b.Start(ctx) }()
	for b.Status().State != BoxStarting {
		time.Sleep(100 * time.Microsecond)
	}
	t0 := time.Now()
	if err := b.Stop(ctx, time.Second); err != nil {
		t.Fatalf("Stop during Start: %v", err)
	}
	if d := time.Since(t0); d > 10*time.Second {
		t.Fatalf("Stop during Start took %v", d)
	}
	if err := <-started; err == nil || !strings.Contains(err.Error(), "stopped while starting") {
		t.Fatalf("Start after a Stop: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if st := b.Status(); st.State != BoxExited {
		t.Fatalf("box after Stop during Start: %s (%s)", st.State, st.Error)
	}
	// The box can start again normally.
	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if st := b.Status(); st.State != BoxRunning {
		t.Fatalf("restart: %s", st.State)
	}
	if err := b.Stop(ctx, time.Second); err != nil {
		t.Fatal(err)
	}
}

// Wait on a box that is created but not started yet returns when the box ends after Start (before, it
// waited on a channel that Start replaced and never closed).
func TestWaitBeforeStart(t *testing.T) {
	e, _ := newEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	b, err := e.Create(ctx, one(t, "../examples/hello.ts", nil))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := b.Wait(ctx)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond) // Wait has taken the channel
	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Wait before Start: %v (box %s)", err, b.Status().State)
	}
}
