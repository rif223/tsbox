package initbox

import (
	"testing"
	"time"
)

func TestHostPaused(t *testing.T) {
	base := time.Now()
	tests := []struct {
		name      string
		now, last time.Time
		want      bool
	}{
		{"normal tick", base.Add(monitorEvery), base, false},
		{"exactly at the gap", base.Add(hostPauseGap), base, false},
		{"standby", base.Add(20 * time.Minute), base, true},
		// Without a monotonic reading: systems where the monotonic clock stops during suspend.
		{"standby on the wall clock", base.Round(0).Add(20 * time.Minute), base.Round(0), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hostPaused(tt.now, tt.last); got != tt.want {
				t.Errorf("hostPaused = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWatchdogExpired(t *testing.T) {
	const wd = 10 * time.Second
	base := time.Now()
	at := func(d time.Duration) time.Time { return base.Add(d) }
	alive := func(d time.Duration) int64 { return at(d).UnixNano() }
	const blocked, noBeat = "event loop blocked", "no heartbeat after the start"

	tests := []struct {
		name      string
		now       time.Time
		notBefore time.Time
		last      int64
		want      string
	}{
		{name: "fresh heartbeat", now: at(time.Minute), notBefore: at(0), last: alive(55 * time.Second)},
		{name: "really blocked", now: at(time.Minute), notBefore: at(0), last: alive(45 * time.Second), want: blocked},
		{name: "starting", now: at(wd + startupGrace), notBefore: at(0)},
		{name: "never sent a heartbeat", now: at(wd + startupGrace + time.Second), notBefore: at(0), want: noBeat},
		{name: "just woke up", now: at(time.Hour), notBefore: at(time.Hour), last: alive(time.Minute)},
		{name: "woke up during the start", now: at(time.Hour), notBefore: at(time.Hour)},
		{name: "silent after waking up", now: at(time.Hour + wd + time.Second), notBefore: at(time.Hour), last: alive(time.Minute), want: blocked},
		{name: "heartbeat after waking up", now: at(time.Hour + wd + time.Second), notBefore: at(time.Hour), last: alive(time.Hour + 5*time.Second)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := watchdogExpired(tt.now, tt.notBefore, tt.last, wd); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
