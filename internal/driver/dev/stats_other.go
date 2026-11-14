//go:build !linux && !windows

package dev

import (
	"time"

	"github.com/rif223/tsbox/internal/driver"
)

// stats: on this system the dev driver measures nothing (no RAM watchdog).
func (*platform) stats(int) (driver.Stats, error) { return driver.Stats{At: time.Now()}, nil }
