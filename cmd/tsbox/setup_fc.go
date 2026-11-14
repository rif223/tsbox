package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rif223/tsbox"
)

// Sources of "tsbox setup --firecracker": the official release (binary, with SHA-256 file) and the guest
// kernels of the Firecracker CI.
const (
	fcReleases = "https://github.com/firecracker-microvm/firecracker/releases"
	fcBucket   = "https://s3.amazonaws.com/spec.ccfc.min"
)

// fcKernelOptions must be built into the guest kernel (no modules).
var fcKernelOptions = []string{"CONFIG_VIRTIO_VSOCKETS", "CONFIG_VIRTIO_BLK", "CONFIG_EXT4_FS", "CONFIG_CGROUPS",
	"CONFIG_MEMCG", "CONFIG_CGROUP_PIDS", "CONFIG_DEVTMPFS", "CONFIG_TMPFS", "CONFIG_SERIAL_8250_CONSOLE"}

var httpc = &http.Client{Timeout: 10 * time.Minute}

func fcArch() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64", nil
	case "arm64":
		return "aarch64", nil
	}
	return "", fmt.Errorf("Firecracker does not support %s", runtime.GOARCH)
}

// setupFirecracker installs the firecracker binary (if missing) and the guest kernel (if missing).
func setupFirecracker(dry bool) error {
	arch, err := fcArch()
	if err != nil {
		return err
	}
	tag, err := latestFirecracker()
	if err != nil {
		return err
	}
	want := map[string]string{} // binary of the release → destination
	bin := envOr("TSBOX_FIRECRACKER", "firecracker")
	if p, err := exec.LookPath(bin); err == nil {
		fmt.Println("== firecracker present:", p)
	} else {
		want["firecracker"] = "/usr/local/bin/firecracker"
	}
	if p, err := exec.LookPath("jailer"); err == nil {
		fmt.Println("== jailer present:", p)
	} else {
		want["jailer"] = "/usr/local/bin/jailer"
	}
	if len(want) > 0 {
		var names []string
		for n, dst := range want {
			names = append(names, n+" to "+dst)
		}
		sort.Strings(names)
		fmt.Printf("== install %s from firecracker %s (%s), SHA-256 checked\n", strings.Join(names, ", "), tag, arch)
		if !dry {
			if err := installFirecracker(tag, arch, want); err != nil {
				return err
			}
		}
	}
	kernel := envOr("TSBOX_FC_KERNEL", tsbox.DefaultFirecrackerKernel)
	if _, err := os.Stat(kernel); err == nil {
		fmt.Println("== guest kernel present:", kernel)
	} else {
		key, err := ciKernel(tag, arch)
		if err != nil {
			return err
		}
		fmt.Printf("== download the guest kernel %s to %s (configuration checked)\n", path.Base(key), kernel)
		if !dry {
			if err := installKernel(key, kernel); err != nil {
				return err
			}
		}
	}
	for _, tool := range []string{"mkfs.ext4", "debugfs"} {
		if _, err := exec.LookPath(tool); err != nil {
			fmt.Printf("== %s missing: install the package e2fsprogs (e.g. apt-get install e2fsprogs)\n", tool)
		}
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		fmt.Println("== warning: /dev/kvm missing - Firecracker needs KVM (Linux on hardware or nested virtualization)")
	}
	return nil
}

// latestFirecracker reads the tag of the latest release from the redirect of /releases/latest.
func latestFirecracker() (string, error) {
	c := *httpc
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Get(fcReleases + "/latest")
	if err != nil {
		return "", fmt.Errorf("firecracker release: %w", err)
	}
	resp.Body.Close()
	tag := path.Base(resp.Header.Get("Location"))
	if !regexp.MustCompile(`^v\d+\.\d+\.\d+$`).MatchString(tag) {
		return "", fmt.Errorf("firecracker release: unexpected answer %q", resp.Header.Get("Location"))
	}
	return tag, nil
}

func get(url string, max int64) ([]byte, error) {
	resp, err := httpc.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err == nil && int64(len(b)) > max {
		err = fmt.Errorf("%s: larger than %d bytes", url, max)
	}
	return b, err
}

// installFirecracker extracts the binaries in bins (release name → destination) from the release archive.
func installFirecracker(tag, arch string, bins map[string]string) error {
	name := fmt.Sprintf("firecracker-%s-%s.tgz", tag, arch)
	tgz, err := get(fcReleases+"/download/"+tag+"/"+name, 256<<20)
	if err != nil {
		return err
	}
	sumFile, err := get(fcReleases+"/download/"+tag+"/"+name+".sha256.txt", 4096)
	if err != nil {
		return err
	}
	want := strings.Fields(string(sumFile))
	got := sha256.Sum256(tgz)
	if len(want) == 0 || !strings.EqualFold(want[0], hex.EncodeToString(got[:])) {
		return fmt.Errorf("%s: SHA-256 does not match", name)
	}
	zr, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	left := len(bins)
	for left > 0 {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%s: %d of the binaries are missing in the archive", name, left)
		}
		if err != nil {
			return err
		}
		for bin, dst := range bins {
			if path.Base(h.Name) == fmt.Sprintf("%s-%s-%s", bin, tag, arch) && h.Typeflag == tar.TypeReg {
				if err := writeAtomic(dst, tr, 0o755); err != nil {
					return err
				}
				left--
			}
		}
	}
	return nil
}

// ciKernel finds the newest 6.x guest kernel in the CI bucket, starting at the release's minor version
// (the bucket lags behind the releases).
func ciKernel(tag, arch string) (string, error) {
	parts := strings.Split(strings.TrimPrefix(tag, "v"), ".")
	minor, _ := strconv.Atoi(parts[1])
	re := regexp.MustCompile(`<Key>(firecracker-ci/v1\.\d+/` + arch + `/vmlinux-6\.\d+\.\d+)</Key>`)
	for m := minor; m >= 8; m-- {
		b, err := get(fmt.Sprintf("%s?prefix=firecracker-ci/v1.%d/%s/vmlinux-6&list-type=2", fcBucket, m, arch), 4<<20)
		if err != nil {
			return "", err
		}
		var keys []string
		for _, x := range re.FindAllStringSubmatch(string(b), -1) {
			keys = append(keys, x[1])
		}
		if len(keys) > 0 {
			sort.Slice(keys, func(i, j int) bool { return versionLess(keys[i], keys[j]) })
			return keys[len(keys)-1], nil
		}
	}
	return "", errors.New("no guest kernel found in the Firecracker CI; set TSBOX_FC_KERNEL to your own")
}

func versionLess(a, b string) bool {
	num := func(s string) []int {
		var out []int
		for _, p := range strings.Split(strings.TrimPrefix(path.Base(s), "vmlinux-"), ".") {
			n, _ := strconv.Atoi(p)
			out = append(out, n)
		}
		return out
	}
	x, y := num(a), num(b)
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] != y[i] {
			return x[i] < y[i]
		}
	}
	return len(x) < len(y)
}

func installKernel(key, dst string) error {
	cfg, err := get(fcBucket+"/"+key+".config", 4<<20)
	if err != nil {
		return err
	}
	for _, opt := range fcKernelOptions {
		if !bytes.Contains(cfg, []byte("\n"+opt+"=y")) {
			return fmt.Errorf("guest kernel %s: %s is not built in", path.Base(key), opt)
		}
	}
	img, err := get(fcBucket+"/"+key, 256<<20)
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(img, []byte("\x7fELF")) {
		return fmt.Errorf("guest kernel %s: not an ELF image", path.Base(key))
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dst+".config", cfg, 0o644); err != nil {
		return err
	}
	return writeAtomic(dst, bytes.NewReader(img), 0o644)
}

func writeAtomic(dst string, r io.Reader, mode os.FileMode) error {
	tmp := dst + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
