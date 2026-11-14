// Package scripts embeds helper scripts into the binary (for "tsbox setup").
package scripts

import _ "embed"

// BuildRootfs is scripts/build-rootfs.sh.
//
//go:embed build-rootfs.sh
var BuildRootfs string
