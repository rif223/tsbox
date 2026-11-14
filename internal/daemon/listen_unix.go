//go:build !windows

package daemon

import (
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

// listen opens the socket. User daemon: directory 0700, socket 0600. System daemon: directory 0750 and
// socket 0660 for root and the group (default "tsbox"); access corresponds to the "docker" group.
func listen(cfg Config) (net.Listener, error) {
	dir := filepath.Dir(cfg.Socket)
	dirMode, sockMode := os.FileMode(0o700), os.FileMode(0o600)
	gid := -1
	if cfg.System {
		dirMode, sockMode = 0o750, 0o660
		group := cfg.Group
		if group == "" {
			group = "tsbox"
		}
		if g, err := user.LookupGroup(group); err == nil {
			gid, _ = strconv.Atoi(g.Gid)
		} else {
			cfg.Logger.Warn("group missing, socket for root only (tsbox setup creates it)", "group", group)
		}
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, err
	}
	if st, err := os.Lstat(dir); err != nil || !st.IsDir() || int(st.Sys().(*syscall.Stat_t).Uid) != os.Geteuid() {
		return nil, fmt.Errorf("socket directory %s does not belong to this user", dir)
	}
	if err := os.Chmod(dir, dirMode); err != nil {
		return nil, err
	}
	if gid >= 0 {
		if err := os.Chown(dir, 0, gid); err != nil {
			return nil, err
		}
	}
	if c, err := net.Dial("unix", cfg.Socket); err == nil {
		c.Close()
		return nil, fmt.Errorf("a tsboxd is already answering on %s", cfg.Socket)
	}
	_ = os.Remove(cfg.Socket)
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", cfg.Socket)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(cfg.Socket, sockMode); err != nil {
		ln.Close()
		return nil, err
	}
	if gid >= 0 {
		if err := os.Chown(cfg.Socket, 0, gid); err != nil {
			ln.Close()
			return nil, err
		}
	}
	return ln, nil
}

// SocketPerm describes the permissions of the socket (for doctor).
func SocketPerm(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	s := st.Sys().(*syscall.Stat_t)
	owner, group := strconv.Itoa(int(s.Uid)), strconv.Itoa(int(s.Gid))
	if u, err := user.LookupId(owner); err == nil {
		owner = u.Username
	}
	if g, err := user.LookupGroupId(group); err == nil {
		group = g.Name
	}
	return fmt.Sprintf("%04o %s:%s", st.Mode().Perm(), owner, group), nil
}

// lockState prevents two daemons on the same state directory.
func lockState(stateDir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(stateDir, "tsboxd.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("already running for %s", stateDir)
	}
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	return func() { f.Close() }, nil
}
