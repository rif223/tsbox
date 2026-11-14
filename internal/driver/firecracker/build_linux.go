//go:build linux

package firecracker

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/rif223/tsbox/internal/build"
	"github.com/rif223/tsbox/internal/fcproto"
)

// Build microVM (like the gVisor build sandbox, P1 S08):
//   - its own VM without network device; deno runs as UID 65534 without capabilities
//   - sources, SDK modules, @tsbox/core and the esbuild cache (DENO_DIR) arrive as files and are read-only
//   - the bundle comes back over vsock
const (
	buildMemory  = 768 << 20
	buildPIDs    = 256
	buildTimeout = 2 * time.Minute
)

// BuildCacheDir is the DENO_DIR of the build VMs (filled once on the host).
func (d *Driver) BuildCacheDir() string { return filepath.Join(d.cfg.StateDir, "denodir") }

// RootDeno is the rootfs's Deno (the box's version).
func (d *Driver) RootDeno() string { return filepath.Join(d.cfg.RootFS, "usr/local/bin/deno") }

// EnsureBuildCache fills the cache once (needs network): a bundle with the rootfs's Deno loads esbuild.
func (d *Driver) EnsureBuildCache(ctx context.Context) error {
	return build.WarmCache(ctx, d.RootDeno(), d.BuildCacheDir())
}

// SetInit sets the tsbox binary that build VMs use as PID 1 (the engine's init binary).
func (d *Driver) SetInit(p string) { d.init = p }

// RunBuild runs inv in a fresh build microVM (signature of build.Runner).
func (d *Driver) RunBuild(ctx context.Context, inv build.Invocation) ([]byte, []byte, error) {
	if d.init == "" {
		return nil, nil, errors.New("firecracker: no init binary for the build VM")
	}
	img, err := d.image(ctx, d.cfg.RootFS, d.init)
	if err != nil {
		return nil, nil, err
	}
	var r [6]byte
	_, _ = rand.Read(r[:])
	ctx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()
	v, err := d.launch(ctx, vmSpec{
		id: "build-" + hex.EncodeToString(r[:]), memory: buildMemory, vcpus: vcpus(1), cpu: 1, rootfs: img,
		header: fcproto.Header{Mode: fcproto.ModeBuild, Hostname: "build", MemoryBytes: buildMemory - guestOverhead,
			PIDs: buildPIDs, Args: inv.Args,
			Env: []string{"NO_COLOR=1", "DENO_DIR=/denodir", "HOME=/tmp", "DENO_NO_UPDATE_CHECK=1"}},
		files: func(tw *tar.Writer) error {
			if err := fcproto.AddTree(tw, d.BuildCacheDir(), "/denodir"); err != nil {
				return err
			}
			for _, m := range inv.Mounts {
				if _, _, err := fcproto.CleanTarget(m.Box, fcproto.BuildRoots); err != nil {
					return err
				}
				if err := fcproto.AddTree(tw, m.Host, m.Box); err != nil {
					return err
				}
			}
			return nil
		},
	})
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		v.kill()
		<-v.done
	}()
	type res struct {
		r        fcproto.Result
		out, err []byte
		e        error
	}
	ch := make(chan res, 1)
	go func() {
		r, out, errb, e := fcproto.ReadResult(v.setup)
		_ = v.setup.Close() // lets the guest end (it waits for this)
		ch <- res{r, out, errb, e}
	}()
	result := func(x res) ([]byte, []byte, error) {
		switch {
		case x.e != nil:
			return nil, nil, fmt.Errorf("firecracker build: %w%s", x.e, v.cons.suffix())
		case x.r.Error != "":
			return x.out, x.err, errors.New(x.r.Error)
		case x.r.Exit != 0:
			return x.out, x.err, fmt.Errorf("exit status %d", x.r.Exit)
		}
		return x.out, x.err, nil
	}
	select {
	case x := <-ch:
		return result(x)
	case <-ctx.Done():
		return nil, nil, fmt.Errorf("build aborted after %v", buildTimeout)
	case <-v.done:
		select {
		case x := <-ch: // the result arrived just before the VM ended
			return result(x)
		case <-time.After(500 * time.Millisecond):
		}
		return nil, nil, fmt.Errorf("firecracker build: the VM ended without a result: %v%s", v.err, v.cons.suffix())
	}
}

// tail keeps the last max bytes of the VM console for error messages.
type tail struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

func (t *tail) suffix() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.b) == 0 {
		return ""
	}
	return "\nVM console:\n" + string(t.b)
}
