// Package cron is the built-in provider of @tsbox/cron: schedules granted by the operator
// (--grant cron:schedule="*/5 * * * *") become "tick" events of the script.
package cron

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed cron expression (5 fields: minute hour day-of-month month day-of-week) or an
// interval (@every 30s).
type Schedule struct {
	minute, hour, dom, month, dow uint64 // bit sets
	domStar, dowStar              bool
	every                         time.Duration
}

// MinEvery is the shortest interval of @every.
const MinEvery = time.Second

var macros = map[string]string{
	"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *", "@weekly": "0 0 * * 0",
	"@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *",
}

var monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8,
	"sep": 9, "oct": 10, "nov": 11, "dec": 12}
var dowNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

// Parse reads a cron expression: "m h dom mon dow" with *, lists (1,2), ranges (1-5), steps (*/15,
// 1-30/5) and names (jan, mon), the macros @hourly, @daily, @weekly, @monthly, @yearly, or "@every <dur>".
func Parse(expr string) (*Schedule, error) {
	expr = strings.TrimSpace(expr)
	if len(expr) > 128 {
		return nil, errors.New("cron: expression longer than 128 characters")
	}
	if d, ok := strings.CutPrefix(expr, "@every "); ok {
		v, err := time.ParseDuration(strings.TrimSpace(d))
		if err != nil {
			return nil, fmt.Errorf("cron: @every: %w", err)
		}
		if v < MinEvery {
			return nil, fmt.Errorf("cron: @every needs at least %v", MinEvery)
		}
		return &Schedule{every: v}, nil
	}
	if m, ok := macros[expr]; ok {
		expr = m
	}
	f := strings.Fields(expr)
	if len(f) != 5 {
		return nil, fmt.Errorf("cron: %q: expected 5 fields (minute hour day month weekday) or @hourly, @daily, @every 30s …", expr)
	}
	s := &Schedule{}
	var err error
	if s.minute, _, err = field(f[0], 0, 59, nil); err != nil {
		return nil, fmt.Errorf("cron: minute: %w", err)
	}
	if s.hour, _, err = field(f[1], 0, 23, nil); err != nil {
		return nil, fmt.Errorf("cron: hour: %w", err)
	}
	if s.dom, s.domStar, err = field(f[2], 1, 31, nil); err != nil {
		return nil, fmt.Errorf("cron: day of month: %w", err)
	}
	if s.month, _, err = field(f[3], 1, 12, monthNames); err != nil {
		return nil, fmt.Errorf("cron: month: %w", err)
	}
	if s.dow, s.dowStar, err = field(f[4], 0, 7, dowNames); err != nil {
		return nil, fmt.Errorf("cron: weekday: %w", err)
	}
	if s.dow&(1<<7) != 0 { // 7 = Sunday
		s.dow |= 1
	}
	return s, nil
}

func field(s string, lo, hi int, names map[string]int) (uint64, bool, error) {
	var bits uint64
	star := false
	for _, part := range strings.Split(s, ",") {
		rng, stepS, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			v, err := strconv.Atoi(stepS)
			if err != nil || v < 1 || v > hi {
				return 0, false, fmt.Errorf("invalid step %q", stepS)
			}
			step = v
		}
		a, b := lo, hi
		switch {
		case rng == "*":
			if !hasStep {
				star = true
			}
		default:
			from, to, isRange := strings.Cut(rng, "-")
			var err error
			if a, err = value(from, lo, hi, names); err != nil {
				return 0, false, err
			}
			b = a
			if isRange {
				if b, err = value(to, lo, hi, names); err != nil {
					return 0, false, err
				}
				if b < a {
					return 0, false, fmt.Errorf("range %q goes backwards", rng)
				}
			} else if hasStep {
				b = hi // "5/10" = from 5 every 10
			}
		}
		for v := a; v <= b; v += step {
			bits |= 1 << uint(v)
		}
	}
	return bits, star, nil
}

func value(s string, lo, hi int, names map[string]int) (int, error) {
	if v, ok := names[strings.ToLower(s)]; ok {
		return v, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < lo || v > hi {
		return 0, fmt.Errorf("value %q outside %d-%d", s, lo, hi)
	}
	return v, nil
}

// Next returns the first time after t that matches (in t's location); zero if there is none within five
// years (e.g. "0 0 30 2 *").
func (s *Schedule) Next(t time.Time) time.Time {
	if s.every > 0 {
		return t.Add(s.every)
	}
	loc := t.Location()
	t = t.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		if s.month&(1<<uint(t.Month())) == 0 {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
			continue
		}
		if !s.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
			continue
		}
		if s.hour&(1<<uint(t.Hour())) == 0 {
			n := time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc)
			if !n.After(t) { // DST: the next hour does not exist in this zone
				n = t.Add(time.Hour).Truncate(time.Hour)
			}
			t = n
			continue
		}
		if s.minute&(1<<uint(t.Minute())) == 0 {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}

// dayMatches: with both day of month and weekday restricted, either may match (as in cron).
func (s *Schedule) dayMatches(t time.Time) bool {
	dom := s.dom&(1<<uint(t.Day())) != 0
	dow := s.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return dow
	case s.dowStar:
		return dom
	}
	return dom || dow
}
