package gvisor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/rif223/tsbox/internal/build"
	"github.com/rif223/tsbox/internal/driver/cgroup"
)

// Build sandbox (P1 S08): deno bundle runs in its own gVisor sandbox
//   - without network (--network=none), as UID 65534 without capabilities
//   - sources, SDK modules and @tsbox/core read-only; no writable host path
//   - esbuild comes from a read-only DENO_DIR (spike e) that tsbox fills once with the rootfs's Deno
//   - the bundle comes back via stdout
const (
	buildMemory  = 768 << 20
	buildPIDs    = 256
	buildTimeout = 2 * time.Minute
	maxBundle    = 16 << 20
)

// BuildCacheDir is the read-only DENO_DIR of the build sandbox.
func (d *Driver) BuildCacheDir() string { return filepath.Join(d.cfg.StateDir, "denodir") }

// RootDeno is the rootfs's Deno (the box's version).
func (d *Driver) RootDeno() string { return filepath.Join(d.cfg.RootFS, "usr/local/bin/deno") }

// EnsureBuildCache fills the cache once (needs network): a bundle with the rootfs's Deno loads esbuild.
func (d *Driver) EnsureBuildCache(ctx context.Context) error {
	return build.WarmCache(ctx, d.RootDeno(), d.BuildCacheDir())
}

// RunBuild runs inv in a fresh build sandbox (signature of build.Runner).
func (d *Driver) RunBuild(ctx context.Context, inv build.Invocation) ([]byte, []byte, error) {
	var r [6]byte
	_, _ = rand.Read(r[:])
	id := "build-" + hex.EncodeToString(r[:])
	bundle := filepath.Join(d.cfg.StateDir, "bundles", id)
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(bundle)

	mounts := []map[string]any{
		{"destination": "/proc", "type": "proc", "source": "proc"},
		{"destination": "/dev", "type": "tmpfs", "source": "tmpfs", "options": []string{"nosuid", "noexec", "mode=755", "size=1m"}},
		{"destination": "/tmp", "type": "tmpfs", "source": "tmpfs", "options": []string{"nosuid", "nodev", "noexec", "mode=1777", "size=64m"}},
		{"destination": "/denodir", "type": "bind", "source": d.BuildCacheDir(), "options": []string{"rbind", "ro", "nosuid", "nodev"}},
	}
	for _, m := range inv.Mounts {
		mounts = append(mounts, map[string]any{"destination": m.Box, "type": "bind", "source": m.Host,
			"options": []string{"rbind", "ro", "nosuid", "nodev", "noexec"}})
	}
	spec := map[string]any{
		"ociVersion": "1.0.2",
		"process": map[string]any{
			"terminal": false,
			"user":     map[string]any{"uid": 65534, "gid": 65534},
			"args":     append([]string{sandboxDeno}, inv.Args...),
			"env":      []string{"NO_COLOR=1", "DENO_DIR=/denodir", "HOME=/tmp", "DENO_NO_UPDATE_CHECK=1"},
			"cwd":      "/tmp",
			"capabilities": map[string]any{
				"bounding": []string{}, "effective": []string{}, "permitted": []string{}, "inheritable": []string{}, "ambient": []string{},
			},
			"noNewPrivileges": true,
			"rlimits":         []map[string]any{{"type": "RLIMIT_NOFILE", "hard": 1024, "soft": 1024}},
		},
		"root":     map[string]any{"path": d.cfg.RootFS, "readonly": true},
		"hostname": id,
		"mounts":   mounts,
		"linux": map[string]any{
			"namespaces":  []map[string]string{{"type": "pid"}, {"type": "ipc"}, {"type": "uts"}, {"type": "mount"}, {"type": "network"}},
			"cgroupsPath": d.cfg.CgroupParent + "/" + id,
			"resources": map[string]any{
				"memory": map[string]any{"limit": buildMemory, "swap": buildMemory},
				"cpu":    map[string]any{"quota": cpuPeriod, "period": cpuPeriod},
				"pids":   map[string]any{"limit": buildPIDs},
			},
		},
	}
	b, _ := json.Marshal(spec)
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), b, 0o600); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()
	cmd := exec.Command(d.cfg.Runsc, append(d.globalFlags(), "run", "--bundle", bundle, id)...)
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitWriter{w: &stdout, n: maxBundle}
	cmd.Stderr = &limitWriter{w: &stderr, n: 1 << 20}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = cgroup.Kill(filepath.Join(cgroup.Root, d.cfg.CgroupParent, id))
		<-done
		err = fmt.Errorf("build aborted after %v", buildTimeout)
	}
	_ = exec.Command(d.cfg.Runsc, append(d.globalFlags(), "delete", "-force", id)...).Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

type limitWriter struct {
	w io.Writer
	n int64
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.n {
		return 0, errors.New("output too large")
	}
	l.n -= int64(len(p))
	return l.w.Write(p)
}
