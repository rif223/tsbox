//go:build !linux

package initbox

// ProbeMain exists only on Linux.
func ProbeMain([]string) int { return 2 }
