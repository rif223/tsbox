//go:build linux

// Package firecracker starts every box in its own Firecracker microVM (KVM). The guest runs tsbox itself as
// PID 1 ("tsbox __fcinit", package fcguest); host and guest talk only over vsock (package fcproto): the
// channel to tsbox-init, the box's files and the measurements. The VM has no network device, its rootfs is
// a read-only ext4 image made from the profile's rootfs directory.
//
// On the host every VM gets its own cgroup (cpu.max, memory.max, pids.max); the box limits (memory, PIDs)
// apply in a cgroup inside the guest, so that the guest kernel ends the largest script on OOM, as under
// gVisor. Builds run in a short-lived microVM of their own (build.go).
package firecracker

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/rif223/tsbox/internal/driver"
	"github.com/rif223/tsbox/internal/driver/cgroup"
	"github.com/rif223/tsbox/internal/fcproto"
)

const (
	// guestOverhead is the memory of the guest kernel and of PID 1 on top of the box limit.
	guestOverhead = 64 << 20
	// vmmOverhead is the memory of the Firecracker process itself (host cgroup).
	vmmOverhead  = 48 << 20
	bootTimeout  = 60 * time.Second
	dirArtifacts = "/artifacts"
)

// KVMDevice is the device Firecracker needs.
const KVMDevice = "/dev/kvm"

// Config of the driver.
type Config struct {
	Firecracker  string // default: firecracker from PATH
	Kernel       string // guest kernel (uncompressed vmlinux with virtio, vsock, ext4, cgroup v2)
	RootFS       string // rootfs directory of the profile "default" (with /usr/local/bin/deno)
	StateDir     string // images, VM directories, build cache
	CgroupParent string // default "/tsbox"
	Jailer       string // opt-in: the jailer binary; "" = without jailer (jail_linux.go)
	JailUID      int    // first UID/GID of the jailed VMMs (default 63000, 1000 slots)
}

// Driver starts microVMs.
type Driver struct {
	cfg    Config
	images imageCache
	init   string // tsbox binary for build VMs (SetInit)
	jails  jailState
}

// New checks the prerequisites: root, /dev/kvm, firecracker, kernel, mkfs.ext4/debugfs and the rootfs.
func New(cfg Config) (*Driver, error) {
	if cfg.Firecracker == "" {
		cfg.Firecracker = "firecracker"
	}
	if cfg.CgroupParent == "" {
		cfg.CgroupParent = "/tsbox"
	}
	if cfg.RootFS == "" || cfg.StateDir == "" {
		return nil, errors.New("firecracker: RootFS and StateDir are required")
	}
	if os.Geteuid() != 0 {
		return nil, errors.New("firecracker: needs root (KVM, cgroups)")
	}
	if err := CheckKVM(); err != nil {
		return nil, err
	}
	p, err := exec.LookPath(cfg.Firecracker)
	if err != nil {
		return nil, fmt.Errorf("firecracker: binary not found (%w); install it or set TSBOX_FIRECRACKER", err)
	}
	cfg.Firecracker = p
	if cfg.Kernel == "" {
		return nil, errors.New("firecracker: no guest kernel; set TSBOX_FC_KERNEL or run \"tsbox setup --firecracker\"")
	}
	if _, err := os.Stat(cfg.Kernel); err != nil {
		return nil, fmt.Errorf("firecracker: guest kernel: %w", err)
	}
	for _, tool := range []string{"mkfs.ext4", "debugfs"} {
		if _, err := exec.LookPath(tool); err != nil {
			return nil, fmt.Errorf("firecracker: %s not found (package e2fsprogs)", tool)
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.RootFS, "usr/local/bin/deno")); err != nil {
		return nil, fmt.Errorf("firecracker: rootfs %s contains no /usr/local/bin/deno", cfg.RootFS)
	}
	if cfg.Jailer != "" {
		p, err := exec.LookPath(cfg.Jailer)
		if err != nil {
			return nil, fmt.Errorf("firecracker: jailer not found (%w); install it or unset TSBOX_FC_JAILER", err)
		}
		cfg.Jailer = p
	}
	return &Driver{cfg: cfg}, nil
}

// CheckKVM reports whether /dev/kvm is usable.
func CheckKVM() error {
	f, err := os.OpenFile(KVMDevice, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("firecracker: %s not usable (%v); Firecracker needs KVM (Linux on hardware or nested virtualization)", KVMDevice, err)
	}
	return f.Close()
}

func (d *Driver) Name() string   { return "firecracker" }
func (d *Driver) Isolated() bool { return true }

// Jailed reports whether the VMMs run under the jailer.
func (d *Driver) Jailed() bool { return d.cfg.Jailer != "" }

// Kernel is the guest kernel in use.
func (d *Driver) Kernel() string { return d.cfg.Kernel }

// vmSpec is what a microVM gets.
type vmSpec struct {
	id      string
	memory  int64 // guest RAM in bytes (box limit + guestOverhead)
	vcpus   int
	cpu     float64 // host cpu.max in cores
	rootfs  string  // image
	stderr  io.Writer
	oomAdj  int
	initBin string // for __oomexec
	header  fcproto.Header
	files   func(*tar.Writer) error
	ports   []uint32 // listeners besides PortSetup
}

// vm is a started microVM.
type vm struct {
	d     *Driver
	id    string
	dir   string
	cgDir string
	cmd   *exec.Cmd
	lns   map[uint32]*net.UnixListener
	setup *net.UnixConn
	cons  *tail // last lines of the console, for error messages
	jail  *jail // nil without jailer

	done chan struct{}
	err  error
	once sync.Once
}

func (d *Driver) launch(ctx context.Context, s vmSpec) (*vm, error) {
	// Paths as Firecracker sees them: on the host, or relative to the chroot of the jailer.
	dir := filepath.Join(d.cfg.StateDir, "vms", s.id)
	kernel, rootfs, uds, cfgArg := d.cfg.Kernel, s.rootfs, filepath.Join(dir, "v.sock"), filepath.Join(dir, "vm.json")
	var j *jail
	if d.Jailed() {
		var err error
		if j, err = d.newJail(s.id, s.rootfs); err != nil {
			return nil, err
		}
		dir = j.root
		kernel, rootfs, uds, cfgArg = "/vmlinux", "/rootfs.ext4", "/v.sock", "/vm.json"
	} else {
		_ = os.RemoveAll(dir)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	v := &vm{d: d, id: s.id, dir: dir, cgDir: filepath.Join(cgroup.Root, d.cfg.CgroupParent, s.id),
		lns: map[uint32]*net.UnixListener{}, done: make(chan struct{}), jail: j}
	ok := false
	defer func() {
		if !ok {
			v.cleanup()
		}
	}()
	for _, port := range append([]uint32{fcproto.PortSetup}, s.ports...) {
		name := filepath.Join(dir, "v.sock_"+strconv.Itoa(int(port)))
		ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: name, Net: "unix"})
		if err != nil {
			return nil, err
		}
		v.lns[port] = ln
		if j != nil {
			if err := os.Chown(name, j.uid, j.uid); err != nil { // the jailed VMM connects to it
				return nil, err
			}
		}
	}
	cfg := map[string]any{
		"boot-source": map[string]any{
			"kernel_image_path": kernel,
			"boot_args": "console=ttyS0 reboot=k panic=1 pci=off nomodules ro root=/dev/vda rootfstype=ext4 " +
				"rootflags=noload quiet loglevel=3 init=/.tsbox/tsbox -- __fcinit",
		},
		"drives":         []map[string]any{{"drive_id": "rootfs", "path_on_host": rootfs, "is_root_device": true, "is_read_only": true}},
		"machine-config": map[string]any{"vcpu_count": s.vcpus, "mem_size_mib": s.memory >> 20, "smt": false},
		"vsock":          map[string]any{"guest_cid": fcproto.GuestCID, "uds_path": uds},
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	cfgPath := filepath.Join(dir, "vm.json")
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		return nil, err
	}
	if j != nil {
		if err := os.Chown(cfgPath, j.uid, j.uid); err != nil {
			return nil, err
		}
	}
	cg, err := v.makeCgroup(s.memory+vmmOverhead, s.cpu)
	if err != nil {
		return nil, err
	}
	defer cg.Close()
	bin, args := d.cfg.Firecracker, []string{"--no-api", "--config-file", cfgArg, "--level", "Error", "--id", s.id}
	if j != nil {
		// The jailer passes --id itself; it creates /dev/kvm in the chroot and execs firecracker as j.uid.
		bin, args = d.cfg.Jailer, d.jailerArgs(s.id, j, []string{"--no-api", "--config-file", cfgArg, "--level", "Error"})
	}
	cmd := exec.Command(bin, args...)
	if s.oomAdj != 0 && s.initBin != "" {
		cmd = exec.Command(s.initBin, append([]string{"__oomexec", strconv.Itoa(s.oomAdj), "--", bin}, args...)...)
	}
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	v.cons = &tail{max: 4096}
	out := io.Writer(v.cons)
	if s.stderr != nil {
		out = io.MultiWriter(v.cons, &consoleWriter{w: s.stderr})
	}
	cmd.Stdout, cmd.Stderr = out, out // serial console (guest kernel, PID 1, init's stderr) and VMM log
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(cg.Fd()), Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting firecracker: %w", err)
	}
	v.cmd = cmd
	go func() {
		err := cmd.Wait()
		v.err = err
		v.finish()
	}()
	ok = true
	conn, err := v.accept(ctx, fcproto.PortSetup)
	if err != nil {
		v.kill()
		return nil, err
	}
	v.setup = conn
	if err := v.send(s.header, s.files); err != nil {
		v.kill()
		<-v.done
		return nil, fmt.Errorf("firecracker %s: sending the files: %w%s", s.id, err, v.cons.suffix())
	}
	return v, nil
}

func (v *vm) makeCgroup(memory int64, cpu float64) (*os.File, error) {
	parent := filepath.Dir(v.cgDir)
	for _, dir := range []string{cgroup.Root, parent} {
		if dir != cgroup.Root {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, err
			}
		}
		_ = os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), []byte("+cpu +memory +pids"), 0)
	}
	_ = os.Remove(v.cgDir) // leftover of an earlier run
	if err := os.Mkdir(v.cgDir, 0o755); err != nil {
		return nil, fmt.Errorf("firecracker: cgroup: %w", err)
	}
	if err := cgroup.Limits(v.cgDir, memory, cpu, 64); err != nil {
		return nil, err
	}
	return os.Open(v.cgDir)
}

// accept waits for the guest's connection to port (until the VM ends or bootTimeout).
func (v *vm) accept(ctx context.Context, port uint32) (*net.UnixConn, error) {
	ln := v.lns[port]
	type res struct {
		c   *net.UnixConn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		_ = ln.SetDeadline(time.Now().Add(bootTimeout))
		for {
			c, err := ln.AcceptUnix()
			if err != nil {
				ch <- res{nil, err}
				return
			}
			// Only a connection that greets is the guest's current one (fcproto.Hello).
			_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
			buf := make([]byte, len(fcproto.Hello))
			if _, err := io.ReadFull(c, buf); err != nil || string(buf) != fcproto.Hello {
				_ = c.Close()
				continue
			}
			_ = c.SetReadDeadline(time.Time{})
			ch <- res{c, nil}
			return
		}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("firecracker %s: the guest did not connect to port %d: %w%s", v.id, port, r.err, v.cons.suffix())
		}
		return r.c, nil
	case <-v.done:
		return nil, fmt.Errorf("firecracker %s: the VM ended during boot: %v%s", v.id, v.err, v.cons.suffix())
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (v *vm) send(h fcproto.Header, files func(*tar.Writer) error) error {
	_ = v.setup.SetWriteDeadline(time.Now().Add(bootTimeout))
	defer v.setup.SetWriteDeadline(time.Time{})
	w := bufio.NewWriterSize(v.setup, 256<<10)
	if err := fcproto.WriteJSON(w, h); err != nil {
		return err
	}
	tw := tar.NewWriter(w)
	if files != nil {
		if err := files(tw); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return w.Flush()
}

func (v *vm) kill() {
	if err := cgroup.Kill(v.cgDir); err != nil && v.cmd != nil && v.cmd.Process != nil {
		_ = v.cmd.Process.Kill()
	}
}

// finish runs once after the Firecracker process ended.
func (v *vm) finish() {
	v.once.Do(func() {
		v.cleanup()
		close(v.done)
	})
}

func (v *vm) cleanup() {
	for _, ln := range v.lns {
		_ = ln.Close()
	}
	if v.setup != nil {
		_ = v.setup.Close()
	}
	if v.jail != nil {
		v.d.freeJail(v.jail) // removes the whole chroot
	} else {
		_ = os.RemoveAll(v.dir)
	}
	// The cgroup can only be removed once all processes have left it.
	for i := 0; i < 50; i++ {
		if err := os.Remove(v.cgDir); err == nil || os.IsNotExist(err) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// vcpus: enough for the CPU limit and at least 2, so that a start boost has an effect.
func vcpus(cpu float64) int {
	n := int(math.Ceil(cpu))
	if n < 2 {
		n = 2
	}
	return min(n, runtime.NumCPU())
}

type box struct {
	*vm
	ch   *net.UnixConn
	st   *net.UnixConn
	stMu sync.Mutex
	stRd *bufio.Reader
}

func (d *Driver) Start(ctx context.Context, spec driver.Spec) (driver.Box, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	rootfs := d.cfg.RootFS
	if spec.RootFS != "" {
		rootfs = spec.RootFS
	}
	img, err := d.image(ctx, rootfs, spec.Init)
	if err != nil {
		return nil, err
	}
	v, err := d.launch(ctx, vmSpec{
		id: spec.ID, memory: spec.Limits.MemoryBytes + guestOverhead, vcpus: vcpus(spec.Limits.CPUCores),
		cpu: spec.Limits.CPUCores, rootfs: img, stderr: spec.Stderr, oomAdj: spec.OOMScoreAdj, initBin: spec.Init,
		header: fcproto.Header{Mode: fcproto.ModeBox, Hostname: spec.ID, MemoryBytes: spec.Limits.MemoryBytes, PIDs: spec.Limits.PIDs},
		files:  func(tw *tar.Writer) error { return fcproto.AddTree(tw, spec.ArtifactsDir, dirArtifacts) },
		ports:  []uint32{fcproto.PortChannel, fcproto.PortStats},
	})
	if err != nil {
		return nil, err
	}
	b := &box{vm: v}
	if b.ch, err = v.accept(ctx, fcproto.PortChannel); err == nil {
		b.st, err = v.accept(ctx, fcproto.PortStats)
	}
	if err != nil {
		v.kill()
		<-v.done
		return nil, err
	}
	b.stRd = bufio.NewReader(b.st)
	return b, nil
}

// halfClose closes only the writing direction: init sees EOF, the host can still read its last frames.
type halfClose struct{ *net.UnixConn }

func (h halfClose) Close() error { return h.CloseWrite() }

func (b *box) Stdin() io.WriteCloser           { return halfClose{b.ch} }
func (b *box) Stdout() io.Reader               { return b.ch }
func (b *box) ArtifactPath(name string) string { return path.Join(dirArtifacts, name) }
func (b *box) UIDMode() bool                   { return true }
func (b *box) Done() <-chan struct{}           { return b.done }
func (b *box) SetCPU(cores float64) error      { return cgroup.SetCPU(b.cgDir, cores) }
func (b *box) Kill() error                     { b.kill(); return nil }

// PID is the host process of the VM (firecracker).
func (b *box) PID() int { return b.cmd.Process.Pid }

func (b *box) Err() error {
	select {
	case <-b.done:
	default:
		return nil
	}
	var ee *exec.ExitError
	if errors.As(b.err, &ee) {
		return fmt.Errorf("firecracker %s ended: %v", b.id, b.err)
	}
	return b.err
}

// Stats: memory, PIDs and OOM kills from the box cgroup in the guest, CPU of the whole VM from the host.
func (b *box) Stats(ctx context.Context) (driver.Stats, error) {
	b.stMu.Lock()
	defer b.stMu.Unlock()
	deadline := time.Now().Add(3 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = b.st.SetDeadline(deadline)
	defer b.st.SetDeadline(time.Time{})
	if _, err := b.st.Write([]byte("s\n")); err != nil {
		return driver.Stats{}, fmt.Errorf("firecracker %s: stats: %w", b.id, err)
	}
	line, err := b.stRd.ReadBytes('\n')
	if err != nil {
		return driver.Stats{}, fmt.Errorf("firecracker %s: stats: %w", b.id, err)
	}
	var gs fcproto.Stats
	if err := json.Unmarshal(line, &gs); err != nil {
		return driver.Stats{}, err
	}
	s := driver.Stats{At: time.Now(), MemoryBytes: gs.MemoryCurrent, MemoryPeak: gs.MemoryPeak, PIDs: gs.PIDs,
		OOMKills: gs.OOMKills, CPUUsage: gs.CPUUsage}
	if hs, err := cgroup.Read(b.cgDir); err == nil {
		s.CPUUsage = hs.CPUUsage
	}
	return s, nil
}

func (b *box) Close() error {
	select {
	case <-b.done:
	default:
		b.kill()
		select {
		case <-b.done:
		case <-time.After(10 * time.Second):
			return fmt.Errorf("firecracker %s: the VM does not end", b.id)
		}
	}
	_ = b.ch.Close()
	_ = b.st.Close()
	return nil
}

// kernelLine are messages of the guest kernel ("[    1.268263] reboot: Restarting system").
var kernelLine = regexp.MustCompile(`^\[\s*\d+\.\d+\]`)

// consoleWriter turns the serial console (arrives byte by byte, with \r\n) into whole lines and drops the
// messages of the guest kernel; what remains is the output of PID 1 and init.
type consoleWriter struct {
	mu  sync.Mutex
	w   io.Writer
	buf []byte
}

func (c *consoleWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf = append(c.buf, p...)
	for {
		i := bytes.IndexByte(c.buf, '\n')
		if i < 0 {
			if len(c.buf) > 64<<10 {
				c.buf = c.buf[:0] // a line without end: discard
			}
			return len(p), nil
		}
		line := bytes.TrimRight(c.buf[:i], "\r")
		c.buf = c.buf[i+1:]
		if len(bytes.TrimSpace(line)) == 0 || kernelLine.Match(line) {
			continue
		}
		_, _ = c.w.Write(append(append([]byte{}, line...), '\n'))
	}
}
