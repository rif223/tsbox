//go:build linux

package firecracker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rif223/tsbox/internal/driver"
	"github.com/rif223/tsbox/internal/profile"
)

// imageVersion changes when the layout of the images changes (mount points, /.tsbox).
const imageVersion = "1"

// guestDirs are mount points PID 1 needs on the read-only rootfs.
var guestDirs = []string{"/.tsbox", "/proc", "/sys", "/dev", "/tmp", "/artifacts", "/work", "/core", "/src", "/sdk", "/denodir"}

type imageCache struct {
	mu     sync.Mutex
	hashes map[string]string // rootfs directory → profile hash (computed once per driver)
	pruned time.Time
}

// imageMaxAge: images not used for this long are removed (an old tsbox version or a removed profile).
// Running VMs are not affected: they keep their open file.
const imageMaxAge = 7 * 24 * time.Hour

// initSuffix names the file next to an image that records the hash of the tsbox binary it contains.
const initSuffix = ".init"

// pruneImages removes unused images, at most once per hour. Called with images.mu held. An image of another
// tsbox binary (initHash differs, or no record: built before records existed) is never used again by this
// binary: it goes at once instead of after imageMaxAge (about 470 MB per tsbox version). Other images go
// after imageMaxAge without use (a removed profile). Running VMs keep their open file.
func (d *Driver) pruneImages(dir, keep, initHash string) {
	if time.Since(d.images.pruned) < time.Hour {
		return
	}
	d.images.pruned = time.Now()
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		info, err := e.Info()
		if err != nil || p == keep || strings.HasSuffix(e.Name(), initSuffix) {
			continue
		}
		rec, rerr := os.ReadFile(p + initSuffix)
		stale := strings.HasSuffix(e.Name(), ".tmp") || time.Since(info.ModTime()) > imageMaxAge ||
			(strings.HasSuffix(e.Name(), ".ext4") && (rerr != nil || strings.TrimSpace(string(rec)) != initHash))
		if stale {
			_ = os.Remove(p)
			_ = os.Remove(p + initSuffix)
		}
	}
}

// image returns the ext4 image of rootfs with the tsbox binary initBin as /.tsbox/tsbox. Images are
// content-addressed (profile hash, tsbox binary, layout) and built once; running VMs keep theirs.
func (d *Driver) image(ctx context.Context, rootfs, initBin string) (string, error) {
	d.images.mu.Lock()
	defer d.images.mu.Unlock()
	if d.images.hashes == nil {
		d.images.hashes = map[string]string{}
	}
	ph, ok := d.images.hashes[rootfs]
	if !ok {
		h, _, err := profile.Hash(rootfs)
		if err != nil {
			return "", fmt.Errorf("firecracker: rootfs %s: %w", rootfs, err)
		}
		ph = h
		d.images.hashes[rootfs] = ph
	}
	ih, err := driver.FileHash(initBin) // cached: hashing the binary took 20–75 ms per start
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(imageVersion + "\x00" + ph + "\x00" + ih))
	dir := filepath.Join(d.cfg.StateDir, "images")
	img := filepath.Join(dir, hex.EncodeToString(sum[:12])+".ext4")
	defer d.pruneImages(dir, img, ih)
	if _, err := os.Stat(img); err == nil {
		now := time.Now()
		_ = os.Chtimes(img, now, now) // last use, for pruneImages
		return img, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := buildImage(ctx, rootfs, initBin, img); err != nil {
		return "", fmt.Errorf("firecracker: building the rootfs image: %w", err)
	}
	_ = os.WriteFile(img+initSuffix, []byte(ih), 0o600)
	return img, nil
}

func buildImage(ctx context.Context, rootfs, initBin, img string) error {
	size, err := treeSize(rootfs)
	if err != nil {
		return err
	}
	st, err := os.Stat(initBin)
	if err != nil {
		return err
	}
	// Payload plus 25 % and 64 MiB for metadata; rounded to MiB.
	total := (size+st.Size())*5/4 + 64<<20
	total = (total + 1<<20 - 1) &^ (1<<20 - 1)
	tmp := img + ".tmp"
	_ = os.Remove(tmp)
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	err = f.Truncate(total)
	f.Close()
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	// Without journal (the image is only ever mounted read-only); the content comes from the directory.
	if out, err := exec.CommandContext(ctx, "mkfs.ext4", "-q", "-F", "-O", "^has_journal", "-L", "tsbox", "-d", rootfs, tmp).CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.ext4: %v: %s", err, out)
	}
	var script strings.Builder
	for _, dir := range guestDirs {
		fmt.Fprintf(&script, "mkdir %s\n", dir)
	}
	fmt.Fprintf(&script, "write %s /.tsbox/tsbox\n", initBin)
	script.WriteString("sif /.tsbox/tsbox mode 0100755\nsif /.tsbox/tsbox uid 0\nsif /.tsbox/tsbox gid 0\n")
	script.WriteString("mknod /dev/console c 5 1\n")
	cmd := exec.CommandContext(ctx, "debugfs", "-w", "-f", "-", tmp)
	cmd.Stdin = strings.NewReader(script.String())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("debugfs: %v: %s", err, out)
	}
	// debugfs reports most errors only as text and exits with 0; check the result.
	check := exec.CommandContext(ctx, "debugfs", "-R", "stat /.tsbox/tsbox", tmp)
	if out, err := check.CombinedOutput(); err != nil || !strings.Contains(string(out), "Size: "+fmt.Sprint(st.Size())) {
		return fmt.Errorf("debugfs: /.tsbox/tsbox missing in the image: %s", out)
	}
	return os.Rename(tmp, img)
}

func treeSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		total += 4096 // inode and directory entries, roughly
		return nil
	})
	return total, err
}
