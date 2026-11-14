//go:build linux

package tsbox

// E2E tests that inspect the box's host processes via /proc: canary in the box memory (I2), oom_score_adj
// of all box processes and named profiles (gVisor).

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf16"
)

// boxPID returns the PID of the box's host process (runsc run, or init in the dev driver).
func boxPID(t *testing.T, b *Box) int {
	t.Helper()
	b.mu.Lock()
	d := b.drv
	b.mu.Unlock()
	p, ok := d.(interface{ PID() int })
	if !ok {
		t.Fatalf("driver box without PID: %T", d)
	}
	return p.PID()
}

// procTree returns root and all descendants (via PPid from /proc/<pid>/stat).
func procTree(root int) []int {
	children := map[int][]int{}
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// pid (comm) state ppid …; comm may contain spaces and parentheses.
		i := bytes.LastIndexByte(b, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(string(b[i+1:]))
		if len(f) < 2 {
			continue
		}
		if ppid, err := strconv.Atoi(f[1]); err == nil {
			children[ppid] = append(children[ppid], pid)
		}
	}
	out := []int{root}
	for i := 0; i < len(out); i++ {
		out = append(out, children[out[i]]...)
	}
	return out
}

func cmdline(pid int) string {
	b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return strings.TrimSpace(string(bytes.ReplaceAll(b, []byte{0}, []byte{' '})))
}

// memScan searches the memory of processes for patterns: all readable mappings via /proc/<pid>/mem and
// all memfd files (under gVisor the box's memory lives in the sentry's memfd "runsc-memory").
type memScan struct {
	needles map[string][]byte
	found   map[string][]string // pattern → locations
	bytes   int64
	seen    map[string]bool // memfd (device:inode)
	errs    []string
}

func newMemScan(needles map[string][]byte) *memScan {
	return &memScan{needles: needles, found: map[string][]string{}, seen: map[string]bool{}}
}

func (m *memScan) maxNeedle() int {
	n := 0
	for _, v := range m.needles {
		n = max(n, len(v))
	}
	return n
}

func (m *memScan) check(buf []byte, where string) {
	for name, n := range m.needles {
		if bytes.Contains(buf, n) {
			m.found[name] = append(m.found[name], where)
		}
	}
}

// scanRange reads [start, end) in chunks with overlap; unreadable chunks are tried page by page.
func (m *memScan) scanRange(r io.ReaderAt, start, end int64, where string) {
	const chunk = 4 << 20
	ov := int64(m.maxNeedle())
	buf := make([]byte, chunk+ov)
	for off := start; off < end; off += chunk {
		n := min(int64(len(buf)), end-off)
		got, err := r.ReadAt(buf[:n], off)
		if err != nil && got == 0 {
			// page by page (guard pages, regions that cannot be mapped)
			for p := off; p < off+min(chunk, end-off); p += 4096 {
				if k, err := r.ReadAt(buf[:4096], p); k > 0 {
					m.bytes += int64(k)
					m.check(buf[:k], where)
				} else if err != nil && len(m.errs) < 5 {
					m.errs = append(m.errs, fmt.Sprintf("%s @%x: %v", where, p, err))
				}
			}
			continue
		}
		m.bytes += int64(got)
		m.check(buf[:got], where)
	}
}

const (
	seekData = 3 // SEEK_DATA
	seekHole = 4 // SEEK_HOLE
)

// proc searches one process.
func (m *memScan) proc(pid int) {
	maps, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil {
		m.errs = append(m.errs, fmt.Sprintf("pid %d maps: %v", pid, err))
		return
	}
	mem, err := os.Open(fmt.Sprintf("/proc/%d/mem", pid))
	if err != nil {
		m.errs = append(m.errs, fmt.Sprintf("pid %d mem: %v", pid, err))
		return
	}
	defer mem.Close()
	for _, line := range strings.Split(string(maps), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || f[1][0] != 'r' {
			continue
		}
		name := ""
		if len(f) > 5 {
			name = f[5]
		}
		if name == "[vvar]" || name == "[vsyscall]" || name == "[vvar_vclock]" {
			continue
		}
		a, b, _ := strings.Cut(f[0], "-")
		start, err1 := strconv.ParseUint(a, 16, 64)
		end, err2 := strconv.ParseUint(b, 16, 64)
		if err1 != nil || err2 != nil || end <= start || end > 1<<62 {
			continue
		}
		m.scanRange(mem, int64(start), int64(end), fmt.Sprintf("pid %d %s %s", pid, f[0], name))
	}
	fds, _ := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	for _, fd := range fds {
		p := fmt.Sprintf("/proc/%d/fd/%s", pid, fd.Name())
		link, err := os.Readlink(p)
		if err != nil || !strings.HasPrefix(link, "/memfd:") {
			continue
		}
		m.memfd(p, fmt.Sprintf("pid %d %s", pid, link))
	}
}

// memfd reads only the data regions (SEEK_DATA/SEEK_HOLE); holes are zeros.
func (m *memScan) memfd(p, where string) {
	f, err := os.Open(p)
	if err != nil {
		m.errs = append(m.errs, fmt.Sprintf("%s: %v", where, err))
		return
	}
	defer f.Close()
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return
	}
	key := fmt.Sprintf("%d:%d", st.Dev, st.Ino)
	if m.seen[key] {
		return
	}
	m.seen[key] = true
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return
	}
	for off := int64(0); off < size; {
		d, err := f.Seek(off, seekData)
		if err != nil {
			break // no more data (ENXIO)
		}
		h, err := f.Seek(d, seekHole)
		if err != nil {
			h = size
		}
		m.scanRange(f, d, h, where)
		off = h
	}
}

func utf16le(s string) []byte {
	var out []byte
	for _, r := range utf16.Encode([]rune(s)) {
		out = append(out, byte(r), byte(r>>8))
	}
	return out
}

// TestCanaryInBoxMemory: invariant I2 in memory – a secret that an SDK provider gets on the host appears
// nowhere in the memory of the box processes (dev: init and Deno; gVisor: runsc, sentry including the box's
// memory, gofer). Positive control: a marker passed on purpose (params) is found.
func TestCanaryInBoxMemory(t *testing.T) {
	e, rec := newEngine(t)
	secret := fmt.Sprintf("tsbox-canary-secret-%d-9f3a71c2", time.Now().UnixNano())
	marker := fmt.Sprintf("tsbox-canary-marker-%d-4be80d17", time.Now().UnixNano())
	spec := BoxSpec{Name: boxFor(t),
		SDKs: []SDKUse{{Name: "./testdata/sdk-secret", Config: map[string]string{"token": "env:TSBOX_CANARY"},
			Env: map[string]string{"TSBOX_CANARY": secret}}},
		Scripts: []ScriptSpec{{Name: "main", Entry: "testdata/scripts/canary.ts",
			Params: json.RawMessage(fmt.Sprintf(`{"marker":%q}`, marker))}}}
	b, err := e.Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Stop(context.Background(), time.Second)
	expectLogs(t, rec, fmt.Sprintf("secret-len %d", len(secret)))
	time.Sleep(500 * time.Millisecond) // let GC/heap settle

	pids := procTree(boxPID(t, b))
	scan := newMemScan(map[string][]byte{
		"marker": []byte(marker), "secret": []byte(secret), "secret-utf16": utf16le(secret),
		"secret-base64": []byte(base64.StdEncoding.EncodeToString([]byte(secret))),
	})
	start := time.Now()
	var procs []string
	for _, pid := range pids {
		procs = append(procs, fmt.Sprintf("%d %s", pid, trunc(cmdline(pid), 60)))
		scan.proc(pid)
	}
	t.Logf("%d processes, %d MiB read in %v: %s", len(pids), scan.bytes>>20, time.Since(start).Round(time.Millisecond), strings.Join(procs, "; "))
	if len(scan.errs) > 0 {
		t.Logf("read errors (excerpt): %s", strings.Join(scan.errs, "; "))
	}
	if len(scan.found["marker"]) == 0 {
		t.Fatalf("positive control: marker not found in the box memory – the scan does not see the box memory")
	}
	t.Logf("marker found in: %s", strings.Join(scan.found["marker"][:min(5, len(scan.found["marker"]))], "; "))
	for _, k := range []string{"secret", "secret-utf16", "secret-base64"} {
		if len(scan.found[k]) > 0 {
			t.Errorf("%s in the box memory: %v", k, scan.found[k])
		}
	}
}

// TestOOMScoreAdj: host OOM hits boxes first. All host processes of the box have the same value (gVisor:
// runsc, sentry, gofer and systrap stubs; dev: init and Deno) so that the cgroup OOM inside the box still
// hits the largest script.
func TestOOMScoreAdj(t *testing.T) {
	e, rec := newEngine(t)
	b, err := e.Run(context.Background(), one(t, "scripts/service.ts", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Stop(context.Background(), time.Second)
	expectLogs(t, rec, "service running")
	var all []string
	sentry, deno, vmm := 0, 0, 0
	for _, pid := range procTree(boxPID(t, b)) {
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/oom_score_adj", pid))
		if err != nil {
			continue // process already ended
		}
		adj, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
		cl := cmdline(pid)
		all = append(all, fmt.Sprintf("%d=%d %s", pid, adj, trunc(cl, 40)))
		if adj != DefaultOOMScoreAdj {
			t.Errorf("%d (%s): oom_score_adj %d", pid, cl, adj)
		}
		if strings.HasPrefix(cl, "runsc-sandbox") {
			sentry++
		}
		if strings.Contains(cl, "deno") {
			deno++
		}
		if strings.Contains(cl, "firecracker") {
			vmm++
		}
	}
	t.Logf("%s", strings.Join(all, "; "))
	if driverName() == "gvisor" && sentry == 0 || driverName() == "dev" && deno == 0 || driverName() == "firecracker" && vmm == 0 {
		t.Fatalf("box processes not found: %s", strings.Join(all, "; "))
	}
}

// TestProfiles (gVisor, Firecracker): a box with a named profile runs on its rootfs; a missing profile
// fails before the build.
func TestProfiles(t *testing.T) {
	needIsolation(t)
	rootfs := os.Getenv("TSBOX_ROOTFS")
	if rootfs == "" {
		rootfs = "/var/lib/tsbox/rootfs"
	}
	dir := t.TempDir()
	if err := os.Symlink(rootfs, filepath.Join(dir, "alt")); err != nil {
		t.Fatal(err)
	}
	e, rec := newEngine(t, WithProfilesDir(dir))
	ps := e.Profiles()
	if len(ps) != 2 || ps[0].Name != "default" || ps[1].Name != "alt" || ps[0].Hash != ps[1].Hash {
		t.Fatalf("Profile: %+v", ps)
	}
	if _, err := e.Create(context.Background(), one(t, "scripts/service.ts", func(s *BoxSpec) { s.Profile = "missing" })); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing profile: %v", err)
	}
	spec := one(t, "scripts/service.ts", func(s *BoxSpec) { s.Profile = "alt" })
	b, err := e.Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Stop(context.Background(), time.Second)
	expectLogs(t, rec, "service running")
	if driverName() == "gvisor" {
		cfg, err := os.ReadFile(filepath.Join(testState, "gvisor", "bundles", spec.Name, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		var oci struct{ Root struct{ Path string } }
		if json.Unmarshal(cfg, &oci); oci.Root.Path != filepath.Join(dir, "alt") {
			t.Fatalf("rootfs of the box %q", oci.Root.Path)
		}
	}
	if st := b.Status(); st.Profile != "alt" || st.ProfileHash != ps[1].Hash {
		t.Fatalf("Status: %s %s", st.Profile, st.ProfileHash)
	}
}

// TestFirecrackerJail (firecracker with TSBOX_FC_JAILER): the VMM runs as an unprivileged UID of its own, in
// a chroot and in a mount namespace of its own; after the box the chroot is gone.
func TestFirecrackerJail(t *testing.T) {
	if driverName() != "firecracker" || os.Getenv("TSBOX_FC_JAILER") == "" {
		t.Skip("only with TSBOX_TEST_DRIVER=firecracker and TSBOX_FC_JAILER")
	}
	e, rec := newEngine(t)
	spec := one(t, "scripts/service.ts", nil)
	b, err := e.Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	expectLogs(t, rec, "service running")
	pid := boxPID(t, b)
	status, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	var uid string
	for _, l := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(l, "Uid:") {
			uid = strings.Fields(l)[1]
		}
	}
	if uid == "" || uid == "0" {
		t.Errorf("VMM runs as UID %q", uid)
	}
	// The jailer uses pivot_root: /proc/<pid>/root shows "/", but its content is the chroot.
	ents, err := os.ReadDir(fmt.Sprintf("/proc/%d/root", pid))
	if err != nil {
		t.Fatal(err)
	}
	var root []string
	for _, e := range ents {
		root = append(root, e.Name())
	}
	if !slices.Contains(root, "rootfs.ext4") || !slices.Contains(root, "vmlinux") || slices.Contains(root, "etc") || slices.Contains(root, "usr") {
		t.Errorf("the VMM's root is not its chroot: %v", root)
	}
	mine, _ := os.Readlink("/proc/self/ns/mnt")
	theirs, _ := os.Readlink(fmt.Sprintf("/proc/%d/ns/mnt", pid))
	if theirs == "" || theirs == mine {
		t.Errorf("VMM shares the mount namespace (%q)", theirs)
	}
	t.Logf("VMM: uid %s, root %s, mnt %s", uid, root, theirs)
	if err := b.Stop(context.Background(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	e.forget(spec.Name, b)
	jail := filepath.Join(testState, "firecracker", "jail", "firecracker", spec.Name)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if _, err := os.Stat(jail); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("chroot %s is still there", jail)
		}
	}
}
