// Package dev starts boxes as normal host processes – for development on Windows, macOS and Linux without
// gVisor. There is NO isolation: the only boundary are the Deno permissions of the script processes.
// Isolated() reports false; with TSBOX_REQUIRE_ISOLATION=1 the engine refuses this driver.
//
// Limits (best effort): Windows enforces RAM, CPU and process count hard via a job object. On
// Linux/macOS a watchdog ends the box when it exceeds its RAM; CPU and PIDs do not apply there.
package dev

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/rif223/tsbox/internal/driver"
)

// ErrMemoryLimit is the reason for ending a box when the watchdog enforces the RAM limit.
var ErrMemoryLimit = driver.ErrBoxOOM

type Driver struct{ watch time.Duration }

func New() *Driver { return &Driver{watch: 250 * time.Millisecond} }

func (d *Driver) Name() string   { return "dev" }
func (d *Driver) Isolated() bool { return false }

type box struct {
	*driver.Process
	spec driver.Spec
	plat *platform

	mu     sync.Mutex
	peak   int64
	oom    int
	reason error
}

func (d *Driver) Start(ctx context.Context, spec driver.Spec) (driver.Box, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if spec.Deno == "" {
		return nil, errors.New("dev: Deno path missing")
	}
	cmd := exec.Command(spec.Init, "__init", "--deno", spec.Deno)
	cmd.Dir = spec.ArtifactsDir
	cmd.Env = minimalEnv()
	prepare(cmd)
	b := &box{spec: spec}
	p, err := driver.StartProcess(cmd, spec.Stderr, nil)
	if err != nil {
		return nil, err
	}
	b.Process = p
	plat, err := attach(cmd, spec.Limits)
	if err != nil {
		_ = cmd.Process.Kill()
		go waitAndRelease(p)
		return nil, fmt.Errorf("dev: setting limits: %w", err)
	}
	// Before the first start frame: init starts scripts only after hello, they inherit the value.
	if err := setOOMScoreAdj(cmd.Process.Pid, spec.OOMScoreAdj); err != nil {
		_ = plat.kill(cmd)
		go waitAndRelease(p)
		return nil, fmt.Errorf("dev: oom_score_adj: %w", err)
	}
	b.plat = plat
	go p.Wait(b.exitErr)
	go b.watchMemory(d.watch)
	return b, nil
}

// waitAndRelease reaps a process whose start failed and closes the read end of its stdout pipe: nobody gets
// the box, so nobody calls Close, which would release it otherwise (one FD per failed start).
func waitAndRelease(p *driver.Process) {
	p.Wait(nil)
	p.Release()
}

func (b *box) exitErr(err error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.reason != nil {
		return b.reason
	}
	return err
}

func (b *box) ArtifactPath(name string) string { return filepath.Join(b.spec.ArtifactsDir, name) }
func (b *box) UIDMode() bool                   { return false }

func (b *box) Stats(context.Context) (driver.Stats, error) {
	st, err := b.plat.stats(b.Cmd.Process.Pid)
	if err != nil {
		return st, err
	}
	b.mu.Lock()
	b.peak = max(b.peak, st.MemoryBytes, st.MemoryPeak)
	st.MemoryPeak, st.OOMKills = b.peak, b.oom
	b.mu.Unlock()
	return st, nil
}

// watchMemory enforces the RAM limit where the system does not do it itself.
func (b *box) watchMemory(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-b.Done():
			return
		case <-t.C:
			st, err := b.plat.stats(b.Cmd.Process.Pid)
			if err != nil || st.MemoryBytes <= b.spec.Limits.MemoryBytes {
				continue
			}
			b.mu.Lock()
			b.oom++
			b.reason = fmt.Errorf("%w: %d MiB > %d MiB", ErrMemoryLimit, st.MemoryBytes>>20, b.spec.Limits.MemoryBytes>>20)
			b.mu.Unlock()
			_ = b.Kill()
			return
		}
	}
}

func (b *box) Kill() error { return b.plat.kill(b.Cmd) }

func (b *box) Close() error {
	_ = b.Kill()
	select {
	case <-b.Done():
	case <-time.After(10 * time.Second):
		return errors.New("dev: box does not end")
	}
	b.Release()
	b.plat.close()
	return nil
}

// SetCPU changes the CPU limit (Windows: job object; elsewhere no effect, dev sets no CPU limit there).
func (b *box) SetCPU(cores float64) error { return b.plat.setCPU(cores) }
