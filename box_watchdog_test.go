package tsbox

import (
	"testing"
	"time"
)

func TestCheckPong(t *testing.T) {
	base := time.Now()
	at := func(d time.Duration) time.Time { return base.Add(d) }

	tests := []struct {
		name              string
		now, tick, pong   time.Time
		wantHung, wantNew bool
	}{
		{name: "answers in time", now: at(10 * time.Second), tick: at(5 * time.Second), pong: at(5 * time.Second)},
		{name: "really hangs", now: at(25 * time.Second), tick: at(20 * time.Second), pong: at(0), wantHung: true},
		{name: "exactly at the deadline", now: at(pongTimeout), tick: at(pongTimeout - pingEvery), pong: at(0)},
		{name: "wakes up from standby", now: at(time.Hour), tick: at(5 * time.Second), pong: at(5 * time.Second), wantNew: true},
		{name: "short pause below the gap", now: at(14 * time.Second), tick: at(2 * time.Second), pong: at(2 * time.Second)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, hung := checkPong(tt.now, tt.tick, tt.pong)
			if hung != tt.wantHung {
				t.Errorf("hung = %v, want %v", hung, tt.wantHung)
			}
			if want := tt.pong; !tt.wantNew && !got.Equal(want) {
				t.Errorf("last pong changed to %v", got)
			}
			if tt.wantNew && !got.Equal(tt.now) {
				t.Errorf("no new deadline after a pause: %v", got)
			}
		})
	}
}

// A clock without monotonic reading stands for systems where the monotonic clock stops during suspend.
func TestCheckPongWallClockGap(t *testing.T) {
	last := time.Now().Round(0)
	now := last.Add(10 * time.Minute)
	if got, hung := checkPong(now, last, last); hung || !got.Equal(now) {
		t.Fatalf("hung = %v, last pong = %v", hung, got)
	}
}

// After a pause the box must still be caught if it stays silent.
func TestCheckPongHungAfterPause(t *testing.T) {
	base := time.Now()
	pong, hung := checkPong(base.Add(time.Hour), base, base)
	if hung {
		t.Fatal("quarantined right after the pause")
	}
	tick := base.Add(time.Hour)
	for tick.Sub(pong) <= pongTimeout {
		next := tick.Add(pingEvery)
		if pong, hung = checkPong(next, tick, pong); hung {
			return
		}
		tick = next
	}
	if _, hung = checkPong(tick.Add(pingEvery), tick, pong); !hung {
		t.Fatal("silent box was not caught after the pause")
	}
}
