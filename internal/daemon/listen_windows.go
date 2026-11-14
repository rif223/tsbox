package daemon

import (
	"fmt"
	"net"
	"path/filepath"
	"syscall"
	"unsafe"

	"github.com/Microsoft/go-winio"

	"github.com/rif223/tsbox/client"
)

// On Windows tsboxd listens on a named pipe (\\.\pipe\tsboxd-<hash>) with a protected DACL for the current
// user only (GENERIC_ALL). Other users can neither connect nor create further instances of the same pipe;
// if the name already exists (foreign pipe), the start fails (first instance).

func listen(cfg Config) (net.Listener, error) {
	sid, err := client.CurrentUserSID()
	if err != nil {
		return nil, err
	}
	if c, err := winio.DialPipe(cfg.Socket, nil); err == nil {
		c.Close()
		return nil, fmt.Errorf("a tsboxd is already answering on %s", cfg.Socket)
	}
	return winio.ListenPipe(cfg.Socket, &winio.PipeConfig{
		SecurityDescriptor: fmt.Sprintf("D:P(A;;GA;;;%s)", sid),
		InputBufferSize:    64 << 10,
		OutputBufferSize:   64 << 10,
	})
}

var (
	advapi32                 = syscall.NewLazyDLL("advapi32.dll")
	procConvertSDToSDDL      = advapi32.NewProc("ConvertSecurityDescriptorToStringSecurityDescriptorW")
	procGetNamedSecurityInfo = advapi32.NewProc("GetNamedSecurityInfoW")
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procLocalFree            = kernel32.NewProc("LocalFree")
)

const (
	seFileObject            = 1
	daclSecurityInformation = 0x4
	sddlRevision1           = 1
)

// SocketPerm describes the pipe's DACL as SDDL (for doctor).
func SocketPerm(path string) (string, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	var sd, dacl uintptr
	if r, _, _ := procGetNamedSecurityInfo.Call(uintptr(unsafe.Pointer(p)), seFileObject, daclSecurityInformation,
		0, 0, uintptr(unsafe.Pointer(&dacl)), 0, uintptr(unsafe.Pointer(&sd))); r != 0 {
		return "", syscall.Errno(r)
	}
	defer procLocalFree.Call(sd)
	var str *uint16
	if r, _, e := procConvertSDToSDDL.Call(sd, sddlRevision1, daclSecurityInformation, uintptr(unsafe.Pointer(&str)), 0); r == 0 {
		return "", e
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(str)))
	return syscall.UTF16ToString(unsafe.Slice(str, 1024)), nil
}

// lockState prevents two daemons on the same state directory (file opened without sharing).
func lockState(stateDir string) (func(), error) {
	p, err := syscall.UTF16PtrFromString(filepath.Join(stateDir, "tsboxd.lock"))
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if err == syscall.Errno(32) { // ERROR_SHARING_VIOLATION
			return nil, fmt.Errorf("already running for %s", stateDir)
		}
		return nil, err
	}
	return func() { syscall.CloseHandle(h) }, nil
}
