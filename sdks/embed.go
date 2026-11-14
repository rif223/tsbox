// Package sdks contains the bundled SDKs (manifest + TS module; providers in internal/providers, or for
// echo an external Deno process).
package sdks

import "embed"

// FS contains log/, kv/, http/ and echo/.
//
//go:embed log kv http echo
var FS embed.FS

// Names are the bundled SDKs.
var Names = []string{"log", "kv", "http", "echo"}
