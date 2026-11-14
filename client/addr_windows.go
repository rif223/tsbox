package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"github.com/Microsoft/go-winio"
)

// On Windows, tsboxd talks over a named pipe (\\.\pipe\tsboxd-<hash>). AF_UNIX would be possible without a
// dependency, but is not usable everywhere (on Windows 11 ARM64, connect fails with WSAEINVAL). The pipe
// has a DACL for the current user only; in addition, the client checks that the server process belongs
// to this user (against foreign pipes with the same name).

// UserSocket is the path of the user daemon's pipe for a state directory.
func UserSocket(stateDir string) string {
	sid, _ := CurrentUserSID()
	sum := sha256.Sum256([]byte(sid + "\x00" + strings.ToLower(filepath.Clean(stateDir))))
	return `\\.\pipe\tsboxd-` + hex.EncodeToString(sum[:6])
}

// UserHost is the address of the user daemon.
func UserHost(stateDir string) string { return "npipe://" + UserSocket(stateDir) }

// CurrentUserSID returns the SID of the current user.
func CurrentUserSID() (string, error) {
	t, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return "", err
	}
	defer t.Close()
	u, err := t.GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String()
}

var (
	kernel32                        = syscall.NewLazyDLL("kernel32.dll")
	procGetNamedPipeServerProcessId = kernel32.NewProc("GetNamedPipeServerProcessId")
)

func dial(ctx context.Context, path string) (net.Conn, error) {
	if !strings.HasPrefix(path, `\\.\pipe\`) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}
	c, err := winio.DialPipeContext(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := checkServerOwner(c); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// checkServerOwner requires that the process behind the pipe belongs to the current user.
func checkServerOwner(c net.Conn) error {
	fd, ok := c.(interface{ Fd() uintptr })
	if !ok {
		return fmt.Errorf("pipe server cannot be checked (%T)", c)
	}
	var pid uint32
	if r, _, e := procGetNamedPipeServerProcessId.Call(fd.Fd(), uintptr(unsafe.Pointer(&pid))); r == 0 {
		return fmt.Errorf("pipe server unknown: %w", e)
	}
	h, err := syscall.OpenProcess(0x1000, false, pid) // PROCESS_QUERY_LIMITED_INFORMATION
	if err != nil {
		return fmt.Errorf("pipe server (PID %d) cannot be checked: %w", pid, err)
	}
	defer syscall.CloseHandle(h)
	var tok syscall.Token
	if err := syscall.OpenProcessToken(h, syscall.TOKEN_QUERY, &tok); err != nil {
		return fmt.Errorf("pipe server (PID %d) cannot be checked: %w", pid, err)
	}
	defer tok.Close()
	u, err := tok.GetTokenUser()
	if err != nil {
		return err
	}
	theirs, _ := u.User.Sid.String()
	mine, err := CurrentUserSID()
	if err != nil {
		return err
	}
	if theirs != mine {
		return fmt.Errorf("pipe is provided by a different user (%s), connection refused", theirs)
	}
	return nil
}
