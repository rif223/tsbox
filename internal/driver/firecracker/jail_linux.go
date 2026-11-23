//go:build linux

package firecracker

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/rif223/tsbox/internal/fsutil"
)

// Jailer (opt-in, Config.Jailer): Firecracker's jailer starts the VMM in a chroot of its own, in its own
// mount namespace and as an unprivileged UID/GID of its own per VM (Config.JailUID + slot). The chroot only
// contains what the VM needs: kernel and rootfs image (hard links), vm.json, the vsock sockets and the
// device nodes the jailer creates (/dev/kvm). A VMM escape then ends up as an unprivileged user in an
// almost empty directory instead of as root on the host.
const (
	defaultJailUID = 63000
	jailSlots      = 1000
)

type jailState struct {
	mu   sync.Mutex
	used map[int]bool
}

// jail is the chroot of one VM.
type jail struct {
	uid  int
	dir  string // <state>/jail/firecracker/<id> (removed afterwards)
	root string // dir/root: the chroot
}

func (d *Driver) jailBase() string { return filepath.Join(d.cfg.StateDir, "jail") }

// newJail prepares the chroot for id: hard links of kernel and image, owner the jail UID.
func (d *Driver) newJail(id, image string) (*jail, error) {
	uid, err := d.allocUID()
	if err != nil {
		return nil, err
	}
	j := &jail{uid: uid, dir: filepath.Join(d.jailBase(), filepath.Base(d.cfg.Firecracker), id)}
	j.root = filepath.Join(j.dir, "root")
	_ = os.RemoveAll(j.dir)
	ok := false
	defer func() {
		if !ok {
			d.freeJail(j)
		}
	}()
	if err := os.MkdirAll(j.root, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chown(j.root, uid, uid); err != nil {
		return nil, err
	}
	kernel, err := d.jailKernel()
	if err != nil {
		return nil, err
	}
	for src, name := range map[string]string{kernel: "vmlinux", image: "rootfs.ext4"} {
		if err := linkOrCopy(src, filepath.Join(j.root, name)); err != nil {
			return nil, fmt.Errorf("firecracker jail: %w", err)
		}
	}
	ok = true
	return j, nil
}

// jailKernel is a copy of the guest kernel in the state directory, so that it can be hard-linked into the
// chroots (the configured kernel may live on another file system).
func (d *Driver) jailKernel() (string, error) {
	h, err := fsutil.FileSHA256(d.cfg.Kernel)
	if err != nil {
		return "", err
	}
	dst := filepath.Join(d.cfg.StateDir, "kernels", h[:16]+".vmlinux")
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	if err := copyFile(d.cfg.Kernel, dst+".tmp"); err != nil {
		return "", err
	}
	return dst, os.Rename(dst+".tmp", dst)
}

func (d *Driver) allocUID() (int, error) {
	d.jails.mu.Lock()
	defer d.jails.mu.Unlock()
	if d.jails.used == nil {
		d.jails.used = map[int]bool{}
	}
	for i := 0; i < jailSlots; i++ {
		if uid := d.jailUID() + i; !d.jails.used[uid] {
			d.jails.used[uid] = true
			return uid, nil
		}
	}
	return 0, fmt.Errorf("firecracker jail: all %d UIDs from %d are in use", jailSlots, d.jailUID())
}

func (d *Driver) jailUID() int {
	if d.cfg.JailUID > 0 {
		return d.cfg.JailUID
	}
	return defaultJailUID
}

func (d *Driver) freeJail(j *jail) {
	_ = os.RemoveAll(j.dir)
	d.jails.mu.Lock()
	delete(d.jails.used, j.uid)
	d.jails.mu.Unlock()
}

// jailerArgs: jailer --id … -- <firecracker arguments>; paths inside are relative to the chroot.
func (d *Driver) jailerArgs(id string, j *jail, fcArgs []string) []string {
	uid := fmt.Sprint(j.uid)
	return append([]string{"--id", id, "--exec-file", d.cfg.Firecracker, "--uid", uid, "--gid", uid,
		"--chroot-base-dir", d.jailBase(), "--"}, fcArgs...)
}

func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return copyFile(src, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
