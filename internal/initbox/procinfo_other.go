//go:build !linux && !windows

package initbox

import (
	"errors"
	"os"
	"syscall"
)

// procInfo: on this system init measures no script values (per-script RSS and thread limits do not apply;
// the driver's box limit remains).
func procInfo(int) (pinfo, error) { return pinfo{}, errors.New("not supported") }

func hasRssAnon() bool { return true }

func signalName(ps *os.ProcessState) string {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return ws.Signal().String()
	}
	return ""
}
