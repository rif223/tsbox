// Package driver is the isolation layer of tsbox: a driver starts a box whose only channel is the
// stdin/stdout of tsbox-init. Drivers: gvisor (Linux, isolated), dev (all systems, NOT isolated).
package driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
)

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)

// ValidID prevents path traversal and invalid cgroup/container names.
func ValidID(id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("invalid box name %q (allowed: a-z, 0-9, -, 1-48 characters)", id)
	}
	return nil
}

// Limits are hard upper limits of the whole box. There are no unlimited boxes.
type Limits struct {
	MemoryBytes int64   `json:"memoryBytes"` // without swap
	CPUCores    float64 `json:"cpuCores"`
	PIDs        int     `json:"pids"` // processes and threads (cgroup pids counts threads)
}

func (l Limits) Validate() error {
	switch {
	case l.MemoryBytes < 32<<20:
		return fmt.Errorf("box memory %s is below the minimum of 32Mi", FormatBytes(l.MemoryBytes))
	case l.CPUCores <= 0 || l.CPUCores > 64:
		return fmt.Errorf("box CPU must be more than 0 and at most 64 cores (got %g)", l.CPUCores)
	case l.PIDs < 8 || l.PIDs > 4096:
		return fmt.Errorf("box PIDs must be between 8 and 4096 (got %d)", l.PIDs)
	}
	return nil
}

// FormatBytes writes a size the way tsbox reads it (64Ki, 1.5Mi, 2Gi, 512B).
func FormatBytes(n int64) string {
	for _, u := range []struct {
		s string
		m int64
	}{{"Gi", 1 << 30}, {"Mi", 1 << 20}, {"Ki", 1 << 10}} {
		if n >= u.m {
			return fmt.Sprintf("%.4g", float64(n)/float64(u.m)) + u.s
		}
	}
	return fmt.Sprintf("%dB", n)
}

// Spec describes a box to start.
type Spec struct {
	ID     string
	Limits Limits
	// ArtifactsDir contains only the artifacts of this box (host path); gvisor mounts it ro as /artifacts.
	ArtifactsDir string
	// Init is the tsbox binary on the host that can run "__init" (usually os.Executable()).
	Init string
	// Deno on the host (dev only; gvisor uses the rootfs's Deno).
	Deno string
	// Stderr receives the output of init itself (not of the scripts). nil = discard.
	Stderr io.Writer
	// RootFS is the rootfs of the profile (gvisor only; "" = the driver's rootfs).
	RootFS string
	// OOMScoreAdj applies to all host processes of the box (gvisor: all runsc processes; dev on Linux: init
	// and scripts) so that the kernel ends boxes first when the host runs out of memory. 0 = unchanged.
	OOMScoreAdj int
}

func (s Spec) Validate() error {
	if err := ValidID(s.ID); err != nil {
		return err
	}
	if err := s.Limits.Validate(); err != nil {
		return err
	}
	if s.Init == "" || s.ArtifactsDir == "" {
		return errors.New("driver: Init and ArtifactsDir are required")
	}
	if s.OOMScoreAdj < -1000 || s.OOMScoreAdj > 1000 {
		return fmt.Errorf("oom_score_adj %d outside [-1000, 1000]", s.OOMScoreAdj)
	}
	return nil
}

// Stats is a measurement of the whole box. Counters are cumulative.
type Stats struct {
	At          time.Time     `json:"at"`
	MemoryBytes int64         `json:"memoryBytes"`
	MemoryPeak  int64         `json:"memoryPeakBytes"`
	CPUUsage    time.Duration `json:"cpuUsageNs"`
	PIDs        int           `json:"pids"`
	OOMKills    int           `json:"oomKills"`
}

// Box is a running box.
type Box interface {
	// Stdin/Stdout are the channel to tsbox-init.
	Stdin() io.WriteCloser
	Stdout() io.Reader
	// ArtifactPath translates the host path of an artifact in ArtifactsDir into init's view.
	ArtifactPath(name string) string
	// UIDMode: init starts scripts with their own UID.
	UIDMode() bool
	Stats(ctx context.Context) (Stats, error)
	// Kill ends the box immediately, even if it hangs.
	Kill() error
	// Done is closed when the box has ended; Err then names the reason (nil = regular).
	Done() <-chan struct{}
	Err() error
	// Close ends the box (if necessary) and cleans everything up. Calling it more than once is allowed.
	Close() error
}

// Driver starts boxes of one isolation technology.
type Driver interface {
	Name() string
	// Isolated reports whether the driver isolates foreign code safely (dev: false).
	Isolated() bool
	Start(ctx context.Context, spec Spec) (Box, error)
}

// ErrBoxOOM: the box reached its memory limit and was ended as a whole (dev driver; under gVisor the
// kernel ends the largest script instead).
var ErrBoxOOM = errors.New("memory limit of the box reached")

// CPUSetter changes the CPU limit of a running box (CPU boost at start). Drivers without an enforced CPU
// limit (dev on Linux/macOS) implement it as a no-op.
type CPUSetter interface {
	SetCPU(cores float64) error
}
