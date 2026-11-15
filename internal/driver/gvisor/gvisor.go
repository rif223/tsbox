// Package gvisor starts boxes with runsc (gVisor). The only channel is the stdin/stdout of "runsc run"
// (--host-uds=none, no socket mount, no writable host path). Linux with cgroup v2 only; runsc runs as
// root. Measurements and background: docs/gvisor-spike.md.
package gvisor

import (
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rif223/tsbox/internal/driver"
	"github.com/rif223/tsbox/internal/driver/cgroup"
)

// Paths in the box.
const (
	dirArtifacts = "/artifacts"
	dirInit      = "/.tsbox"
	sandboxDeno  = "/usr/local/bin/deno"
)

// Config of the driver.
type Config struct {
	Runsc        string // default: runsc from PATH (sidecars in gvisor-bin/ next to it)
	RootFS       string // read-only rootfs with /usr/local/bin/deno
	StateDir     string // bundles, runsc state, copies of tsbox-init
	CgroupParent string // default "/tsbox"
	Platform     string // "systrap" (default) or "kvm"
}

type Driver struct{ cfg Config }

func New(cfg Config) (*Driver, error) {
	if cfg.Runsc == "" {
		cfg.Runsc = "runsc"
	}
	if cfg.Platform == "" {
		cfg.Platform = "systrap"
	}
	if err := checkPlatform(cfg.Platform); err != nil {
		return nil, err
	}
	if cfg.CgroupParent == "" {
		cfg.CgroupParent = "/tsbox"
	}
	if cfg.RootFS == "" || cfg.StateDir == "" {
		return nil, errors.New("gvisor: RootFS and StateDir are required")
	}
	p, err := exec.LookPath(cfg.Runsc)
	if err != nil {
		return nil, fmt.Errorf("gvisor: runsc not found (%w). Install gVisor or use --driver dev (no isolation)", err)
	}
	cfg.Runsc = p
	if _, err := os.Stat(filepath.Join(cfg.RootFS, "usr/local/bin/deno")); err != nil {
		return nil, fmt.Errorf("gvisor: rootfs %s contains no /usr/local/bin/deno", cfg.RootFS)
	}
	if os.Geteuid() != 0 {
		return nil, errors.New("gvisor: runsc needs root (cgroups, namespaces)")
	}
	return &Driver{cfg: cfg}, nil
}

func (d *Driver) Name() string   { return "gvisor" }
func (d *Driver) Isolated() bool { return true }

func (d *Driver) globalFlags() []string {
	flags := []string{
		"--root=" + filepath.Join(d.cfg.StateDir, "runsc"),
		"--network=none",
		"--platform=" + d.cfg.Platform,
		"--directfs",
		"--host-uds=none", // channel over stdio, no host sockets (spike a)
		// No overlay: otherwise runsc puts a writable layer (in memory) over the rootfs, and the rootfs's /tmp
		// would be writable for all scripts – despite root.readonly (found by the escape simulation).
		"--overlay2=none",
	}
	// Diagnosis only (e.g. CI): runsc writes its debug logs (one file per command and sandbox) into this
	// directory. They can be large and contain script output; never set in production.
	if dir := os.Getenv("TSBOX_RUNSC_DEBUG"); dir != "" {
		flags = append(flags, "--debug", "--debug-log="+strings.TrimSuffix(dir, "/")+"/", "--panic-log="+strings.TrimSuffix(dir, "/")+"/panic-%ID%.log")
	}
	return flags
}

type box struct {
	*driver.Process
	d     *Driver
	id    string
	cgDir string
}

func (d *Driver) Start(ctx context.Context, spec driver.Spec) (driver.Box, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if spec.RootFS != "" {
		if _, err := os.Stat(filepath.Join(spec.RootFS, "usr/local/bin/deno")); err != nil {
			return nil, fmt.Errorf("gvisor: rootfs %s contains no /usr/local/bin/deno", spec.RootFS)
		}
	}
	initDir, err := d.installInit(spec.Init)
	if err != nil {
		return nil, err
	}
	bundle := filepath.Join(d.cfg.StateDir, "bundles", spec.ID)
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(ociSpec(d.cfg, spec, initDir), "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), b, 0o600); err != nil {
		return nil, err
	}
	// Remove leftovers of an earlier run with the same ID (e.g. after tsbox crashed).
	_ = exec.Command(d.cfg.Runsc, append(d.globalFlags(), "delete", "-force", spec.ID)...).Run()

	bx := &box{d: d, id: spec.ID, cgDir: filepath.Join(cgroup.Root, d.cfg.CgroupParent, spec.ID)}
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, d.cfg.Runsc, append(d.globalFlags(), "delete", "-force", spec.ID)...).Run()
		_ = os.RemoveAll(bundle)
	}
	// "run" without -detach: runsc lives as long as the box, stdin/stdout are the channel to init.
	args := append(d.globalFlags(), "run", "--bundle", bundle, spec.ID)
	cmd := exec.Command(d.cfg.Runsc, args...)
	if spec.OOMScoreAdj != 0 {
		// Via "tsbox __oomexec" all processes of the box (sentry, gofer, systrap stubs) inherit the value from
		// the start. Via the OCI spec only sentry and gofer would get it, not the stubs – then the cgroup OOM
		// would hit the sentry (the whole box) instead of the largest script (found by TestBoxOOM).
		cmd = exec.Command(spec.Init, append([]string{"__oomexec", strconv.Itoa(spec.OOMScoreAdj), "--", d.cfg.Runsc}, args...)...)
	}
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	p, err := driver.StartProcess(cmd, spec.Stderr, cleanup)
	if err != nil {
		return nil, err
	}
	bx.Process = p
	go p.Wait(nil)
	return bx, nil
}

// installInit stores a copy of the (static) tsbox binary content-addressed. Running boxes keep their
// version even when tsbox is updated.
func (d *Driver) installInit(src string) (string, error) {
	if err := requireStatic(src); err != nil {
		return "", err
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	dir := filepath.Join(d.cfg.StateDir, "init", hex.EncodeToString(sum[:8]))
	dst := filepath.Join(dir, "tsbox")
	if _, err := os.Stat(dst); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, 0o555); err != nil {
		return "", err
	}
	return dir, os.Rename(tmp, dst)
}

// requireStatic: the binary runs in the box's rootfs and must not need host libraries.
func requireStatic(p string) error {
	f, err := elf.Open(p)
	if err != nil {
		return fmt.Errorf("gvisor: %s is not a Linux binary: %w", p, err)
	}
	defer f.Close()
	for _, prog := range f.Progs {
		if prog.Type == elf.PT_INTERP {
			return fmt.Errorf("gvisor: %s is dynamically linked; build tsbox with CGO_ENABLED=0", p)
		}
	}
	return nil
}

func (b *box) ArtifactPath(name string) string { return path.Join(dirArtifacts, name) }
func (b *box) UIDMode() bool                   { return true }

func (b *box) Stats(context.Context) (driver.Stats, error) {
	cs, err := cgroup.Read(b.cgDir)
	if err != nil {
		return driver.Stats{}, fmt.Errorf("gvisor %s: stats: %w", b.id, err)
	}
	return driver.Stats{
		At: time.Now(), MemoryBytes: cs.MemoryCurrent, MemoryPeak: cs.MemoryPeak,
		CPUUsage: cs.CPUUsage, PIDs: cs.PIDs, OOMKills: cs.OOMKills,
	}, nil
}

// Kill ends all processes of the box via cgroup.kill; fallback: the runsc process.
func (b *box) Kill() error {
	if err := cgroup.Kill(b.cgDir); err != nil {
		if b.Cmd.Process != nil {
			return b.Cmd.Process.Kill()
		}
		return err
	}
	return nil
}

func (b *box) Close() error {
	select {
	case <-b.Done():
	default:
		_ = b.Kill()
		select {
		case <-b.Done():
		case <-time.After(10 * time.Second):
			return fmt.Errorf("gvisor %s: box does not end", b.id)
		}
	}
	b.Release()
	return nil
}

// KVMDevice is the device the kvm platform needs.
var KVMDevice = "/dev/kvm"

// checkPlatform: systrap runs everywhere; kvm needs access to /dev/kvm (bare metal or nested
// virtualization) and speeds up system calls and page faults.
func checkPlatform(p string) error {
	switch p {
	case "systrap":
		return nil
	case "kvm":
		f, err := os.OpenFile(KVMDevice, os.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("gvisor: platform kvm needs %s (%v); without KVM use platform systrap", KVMDevice, err)
		}
		f.Close()
		return nil
	}
	return fmt.Errorf("gvisor: unknown platform %q (systrap, kvm)", p)
}

// Platform is the selected gVisor platform.
func (d *Driver) Platform() string { return d.cfg.Platform }

// SetCPU sets the box's cpu.max anew.
func (b *box) SetCPU(cores float64) error { return cgroup.SetCPU(b.cgDir, cores) }
