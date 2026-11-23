// Package fcproto is the small protocol between the Firecracker driver on the host and tsbox's PID 1 in the
// microVM (fcguest). All connections go from the guest to the host over vsock; Firecracker forwards a
// guest connection to port P to the host Unix socket "<uds_path>_P".
//
//   - PortSetup: the host sends a Header (u32 big endian length | JSON) and then a tar stream with the files
//     the guest needs (artifacts, or the inputs of a build). A build sends its Result back on this
//     connection.
//   - PortChannel (box mode): the channel to tsbox-init (stdin/stdout of "tsbox __init"), unchanged frames.
//   - PortStats (box mode): the host writes "s\n", the guest answers one JSON line (Stats).
package fcproto

import (
	"archive/tar"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/rif223/tsbox/internal/fsutil"
)

// Ports and CIDs.
const (
	PortSetup   = 1024
	PortChannel = 1025
	PortStats   = 1026
	GuestCID    = 3
)

// Hello is the first thing the guest writes on every connection. The host only uses connections that
// start with it: a guest connect that timed out on the guest side may still arrive at the host later.
const Hello = "tsbox-vsock-1\n"

// Modes of a microVM.
const (
	ModeBox   = "box"
	ModeBuild = "build"
)

// Limits of what a guest accepts from the host.
const (
	MaxHeader = 1 << 20
	MaxFiles  = 20000
	MaxTree   = 512 << 20 // sum of all file sizes in the tar stream
	MaxResult = 32 << 20
)

// BoxRoots are the guest directories the host may fill in box mode, BuildRoots those of a build.
var (
	BoxRoots   = []string{"/artifacts"}
	BuildRoots = []string{"/work", "/core", "/src", "/sdk", "/denodir"}
)

// Header describes the task of a microVM.
type Header struct {
	Mode     string `json:"mode"`
	Hostname string `json:"hostname"`
	// Box mode: limits of the box cgroup in the guest (all scripts together).
	MemoryBytes int64 `json:"memoryBytes,omitempty"`
	PIDs        int   `json:"pids,omitempty"`
	// Build mode: Deno arguments and environment; the process runs as UID/GID 65534 in /tmp.
	Args []string `json:"args,omitempty"`
	Env  []string `json:"env,omitempty"`
}

// Stats are the values of the box cgroup in the guest.
type Stats struct {
	MemoryCurrent int64         `json:"memoryCurrent"`
	MemoryPeak    int64         `json:"memoryPeak"`
	OOMKills      int           `json:"oomKills"`
	PIDs          int           `json:"pids"`
	CPUUsage      time.Duration `json:"cpuUsageNs"`
}

// Result of a build.
type Result struct {
	Exit      int    `json:"exit"`
	Error     string `json:"error,omitempty"` // the guest could not run the command at all
	StdoutLen int    `json:"stdoutLen"`
	StderrLen int    `json:"stderrLen"`
}

// WriteJSON writes v as u32 big endian length followed by JSON.
func WriteJSON(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(b)))
	if _, err := w.Write(n[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// ReadJSON reads a value written by WriteJSON (at most max bytes).
func ReadJSON(r io.Reader, max int, v any) error {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return err
	}
	size := int(binary.BigEndian.Uint32(n[:]))
	if size > max {
		return fmt.Errorf("fcproto: message of %d bytes is too large", size)
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// WriteResult sends a build result followed by stdout and stderr.
func WriteResult(w io.Writer, res Result, stdout, stderr []byte) error {
	res.StdoutLen, res.StderrLen = len(stdout), len(stderr)
	if err := WriteJSON(w, res); err != nil {
		return err
	}
	if _, err := w.Write(stdout); err != nil {
		return err
	}
	_, err := w.Write(stderr)
	return err
}

// ReadResult reads a build result.
func ReadResult(r io.Reader) (Result, []byte, []byte, error) {
	var res Result
	if err := ReadJSON(r, MaxHeader, &res); err != nil {
		return res, nil, nil, err
	}
	if res.StdoutLen < 0 || res.StderrLen < 0 || res.StdoutLen+res.StderrLen > MaxResult {
		return res, nil, nil, errors.New("fcproto: result too large")
	}
	out := make([]byte, res.StdoutLen)
	if _, err := io.ReadFull(r, out); err != nil {
		return res, nil, nil, err
	}
	errb := make([]byte, res.StderrLen)
	if _, err := io.ReadFull(r, errb); err != nil {
		return res, nil, nil, err
	}
	return res, out, errb, nil
}

// AddTree writes the regular files and directories below hostDir into tw under the guest path guestDir.
// Symlinks are followed only if they stay inside hostDir; other special files are skipped.
func AddTree(tw *tar.Writer, hostDir, guestDir string) error {
	root, err := filepath.Abs(hostDir)
	if err != nil {
		return err
	}
	// Link targets are compared with the resolved root: if the root itself lies below a symlink (macOS:
	// /var -> /private/var), EvalSymlinks of a link inside the tree would otherwise look like "outside".
	realRoot := root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		realRoot = r
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		name := path.Join(guestDir, filepath.ToSlash(rel))
		info, err := os.Stat(p) // follows symlinks
		if err != nil {
			return nil // dangling symlink
		}
		if d.Type()&fs.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(p)
			if err != nil || !fsutil.Within(realRoot, target) {
				return nil
			}
		}
		switch {
		case info.IsDir():
			if d.Type()&fs.ModeSymlink != 0 {
				return nil // a symlinked directory is not followed (loops)
			}
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: 0o755})
		case info.Mode().IsRegular():
			mode := int64(0o644)
			if info.Mode()&0o111 != 0 {
				mode = 0o755
			}
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: info.Size()}); err != nil {
				return err
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.CopyN(tw, f, info.Size())
			return err
		}
		return nil
	})
}

// CleanTarget checks a tar entry name: an absolute, clean path below one of roots. It returns the root and
// the path.
func CleanTarget(name string, roots []string) (string, string, error) {
	clean := path.Clean("/" + strings.TrimPrefix(name, "/"))
	if strings.Contains(name, "..") || clean == "/" {
		return "", "", fmt.Errorf("fcproto: invalid path %q", name)
	}
	for _, r := range roots {
		if clean == r || strings.HasPrefix(clean, r+"/") {
			return r, clean, nil
		}
	}
	return "", "", fmt.Errorf("fcproto: path %q outside %v", name, roots)
}

// TreeSize counts what AddTree would send for hostDir: the sum of the file sizes and the number of entries.
func TreeSize(hostDir string) (bytes int64, files int, err error) {
	root, err := filepath.Abs(hostDir)
	if err != nil {
		return 0, 0, err
	}
	realRoot := root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		realRoot = r
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Stat(p)
		if err != nil {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(p)
			if err != nil || !fsutil.Within(realRoot, target) || info.IsDir() {
				return nil
			}
		}
		files++
		if info.Mode().IsRegular() {
			bytes += info.Size()
		}
		return nil
	})
	return bytes, files, err
}
