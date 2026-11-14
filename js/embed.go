// Package js contains @tsbox/core, the TypeScript module bundled into every script.
package js

import "embed"

// Core holds the sources of @tsbox/core (core/*.ts).
//
//go:embed core/*.ts
var Core embed.FS
