package cron

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rif223/tsbox/internal/sdktest"
	"github.com/rif223/tsbox/sdk"
)

func TestNext(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("no time zone data:", err)
	}
	at := func(s string, loc *time.Location) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct {
		expr, from, want string
		loc              *time.Location
	}{
		{"*/15 * * * *", "2026-11-28 10:07", "2026-11-28 10:15", time.UTC},
		{"0 3 * * *", "2026-11-28 03:00", "2026-11-29 03:00", time.UTC},
		{"30 9 * * mon-fri", "2026-11-28 10:00", "2026-11-30 09:30", time.UTC}, // Saturday → Monday
		{"0 0 1 */3 *", "2026-11-28 00:00", "2027-01-01 00:00", time.UTC},
		{"0 12 13 * fri", "2026-11-28 00:00", "2026-12-04 12:00", time.UTC}, // dom OR dow
		{"0 0 * * 7", "2026-11-28 00:00", "2026-11-29 00:00", time.UTC},     // 7 = Sunday
		{"@monthly", "2026-11-28 00:00", "2026-12-01 00:00", time.UTC},
		{"5-10/5 1 * jan,jul *", "2026-11-28 00:00", "2027-01-01 01:05", time.UTC},
		{"30 2 * * *", "2027-03-27 12:00", "2027-03-29 02:30", berlin}, // 02:30 does not exist on 28 March
		{"0 3 * * *", "2027-03-28 00:00", "2027-03-28 03:00", berlin},
	} {
		s, err := Parse(c.expr)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		got := s.Next(at(c.from, c.loc))
		if want := at(c.want, c.loc); !got.Equal(want) {
			t.Errorf("%s after %s: %s, want %s", c.expr, c.from, got, want)
		}
	}
	if s, _ := Parse("0 0 30 2 *"); !s.Next(time.Now()).IsZero() {
		t.Error("30 February matched")
	}
	for _, bad := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "*/0 * * * *", "5-1 * * * *",
		"@every 10ms", "@every x", "@sometimes", "* * * foo *"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestProviderTicks(t *testing.T) {
	p := New()
	g := sdk.Grants{"schedule": {"fast=@every 1s", "0 3 * * *"}}
	if err := p.ValidateGrants(g, sdk.Config{"timezone": "UTC"}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []sdk.Grants{{"schedule": {"nope"}}, {"schedule": {"a=@daily", "a=@hourly"}}} {
		if err := p.ValidateGrants(bad, sdk.Config{}); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if err := p.ValidateGrants(g, sdk.Config{"timezone": "Mars/Olympus"}); err == nil {
		t.Error("unknown time zone accepted")
	}
	s := &sdktest.Session{G: g, C: sdk.Config{"timezone": "UTC"}}
	h, err := p.Open(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	r, err := h.Call(context.Background(), "next", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(r)
	var out struct {
		Schedules []struct {
			Name string `json:"name"`
			Next int64  `json:"next"`
		} `json:"schedules"`
	}
	if json.Unmarshal(b, &out); len(out.Schedules) != 2 || out.Schedules[0].Name != "0 3 * * *" || out.Schedules[1].Next == 0 {
		t.Fatalf("schedules: %s", b)
	}
	time.Sleep(2300 * time.Millisecond)
	h.Close()
	s2 := len(s.Events)
	if s2 < 1 || s2 > 3 {
		t.Fatalf("ticks after 2.3 s with @every 1s: %d", s2)
	}
	time.Sleep(1200 * time.Millisecond)
	if len(s.Events) != s2 {
		t.Fatal("ticks after Close")
	}
}
