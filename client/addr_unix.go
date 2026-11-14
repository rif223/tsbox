//go:build !windows

package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
)

// maxSocketPath keeps a margin to the AF_UNIX limit (108 bytes including the null byte).
const maxSocketPath = 100

// UserSocket is the socket of the user daemon for a state directory: <state>/run/tsboxd.sock.
// If the path is too long for AF_UNIX, the socket lives in a short, user-owned directory
// ($XDG_RUNTIME_DIR/tsbox or /tmp/tsbox-<uid>; the daemon checks the owner) under a name derived from the
// hash of the state directory.
func UserSocket(stateDir string) string {
	p := filepath.Join(stateDir, "run", "tsboxd.sock")
	if len(p) <= maxSocketPath {
		return p
	}
	sum := sha256.Sum256([]byte(filepath.Clean(stateDir)))
	name := hex.EncodeToString(sum[:6]) + ".sock"
	dir := filepath.Join("/tmp", "tsbox-"+strconv.Itoa(os.Getuid()))
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		dir = filepath.Join(x, "tsbox")
	}
	return filepath.Join(dir, name)
}

// UserHost is the address of the user daemon.
func UserHost(stateDir string) string { return "unix://" + UserSocket(stateDir) }

func dial(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", path)
}

// CurrentUserSID exists only on Windows.
func CurrentUserSID() (string, error) { return "", errors.New("SIDs exist only on Windows") }
