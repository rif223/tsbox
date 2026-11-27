package cron

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rif223/tsbox/sdk"
)

// MaxSchedules per script.
const MaxSchedules = 32

var namePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.-]{0,63}$`)

// entry is a granted schedule: "nightly=0 3 * * *" or only the expression (then its name).
type entry struct {
	name, expr string
	s          *Schedule
}

func parseGrants(vals []string) ([]entry, error) {
	if len(vals) > MaxSchedules {
		return nil, fmt.Errorf("cron: at most %d schedules", MaxSchedules)
	}
	var out []entry
	seen := map[string]bool{}
	for _, v := range vals {
		name, expr := v, v
		if n, e, ok := strings.Cut(v, "="); ok && namePattern.MatchString(n) {
			name, expr = n, e
		}
		s, err := Parse(expr)
		if err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, fmt.Errorf("cron: schedule %q granted twice", name)
		}
		seen[name] = true
		out = append(out, entry{name: name, expr: strings.TrimSpace(expr), s: s})
	}
	return out, nil
}

func location(c sdk.Config) (*time.Location, error) {
	tz := c.String("timezone")
	if tz == "" || tz == "UTC" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("cron: timezone %q: %w", tz, err)
	}
	return loc, nil
}

// Provider emits a "tick" event per granted schedule.
type Provider struct {
	now func() time.Time
}

// New creates the provider.
func New() *Provider { return &Provider{now: time.Now} }

// ValidateGrants checks the schedules when the box is created (sdk.GrantValidator).
func (p *Provider) ValidateGrants(g sdk.Grants, c sdk.Config) error {
	if _, err := location(c); err != nil {
		return err
	}
	_, err := parseGrants(g.Values("schedule"))
	return err
}

type handler struct {
	*sdk.Router
	stop chan struct{}
	once sync.Once
	wg   sync.WaitGroup
}

func (h *handler) Close() error {
	h.once.Do(func() { close(h.stop) })
	h.wg.Wait()
	return nil
}

func (p *Provider) Open(_ context.Context, s sdk.Session) (sdk.Handler, error) {
	entries, err := parseGrants(s.Grants().Values("schedule"))
	if err != nil {
		return nil, err
	}
	loc, err := location(s.Config())
	if err != nil {
		return nil, err
	}
	h := &handler{Router: sdk.NewRouter(s), stop: make(chan struct{})}
	sdk.Method(h.Router, "next", func(_ context.Context, _ sdk.Session, _ struct{}) (any, error) {
		now := p.now().In(loc)
		type item struct {
			Name     string `json:"name"`
			Schedule string `json:"schedule"`
			Next     int64  `json:"next,omitempty"`
		}
		out := []item{}
		for _, e := range entries {
			it := item{Name: e.name, Schedule: e.expr}
			if n := e.s.Next(now); !n.IsZero() {
				it.Next = n.UnixMilli()
			}
			out = append(out, it)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return map[string]any{"schedules": out, "timezone": loc.String()}, nil
	})
	for _, e := range entries {
		h.wg.Add(1)
		go p.loop(h, s, e, loc)
	}
	return h, nil
}

func (p *Provider) loop(h *handler, s sdk.Session, e entry, loc *time.Location) {
	defer h.wg.Done()
	last := p.now().In(loc)
	for {
		next := e.s.Next(last)
		if next.IsZero() {
			return
		}
		t := time.NewTimer(time.Until(next))
		select {
		case <-h.stop:
			t.Stop()
			return
		case <-t.C:
		}
		now := p.now().In(loc)
		if now.Before(next) { // timer fired early (clock changed): wait for the rest
			last = now.Add(-time.Nanosecond)
			if e.s.every > 0 {
				last = next.Add(-e.s.every)
			}
			continue
		}
		err := s.Emit("tick", map[string]any{"name": e.name, "schedule": e.expr, "scheduled": next.UnixMilli(), "at": now.UnixMilli()})
		if err != nil && !errors.Is(err, sdk.ErrNotSubscribe) && !errors.Is(err, sdk.ErrQueueFull) {
			s.Logger().Warn("cron tick", "schedule", e.name, "err", err)
		}
		// After a pause (sleep), skipped runs are not repeated: continue from now.
		last = next
		if now.Sub(next) > time.Minute {
			last = now
		}
	}
}
