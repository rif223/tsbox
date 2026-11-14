//go:build !linux

package initbox

import (
	"fmt"
	"os"
)

// ExecMain exists only on Linux (UID mode in gVisor).
func ExecMain([]string) int {
	fmt.Fprintln(os.Stderr, "tsbox __exec: Linux only")
	return 2
}

// OOMExecMain exists only on Linux (gVisor driver).
func OOMExecMain([]string) int {
	fmt.Fprintln(os.Stderr, "tsbox __oomexec: Linux only")
	return 2
}
