package main

import (
	"testing"
)

// FuzzSince: --since from the command line.
func FuzzSince(f *testing.F) {
	for _, s := range []string{"10m", "2026-11-02", "2026-11-02T10:00:00Z", "-5h", "9999999999h"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = parseSince(s)
	})
}

// FuzzSDKFlags: --sdk, --grant and --sdk-config; grants and settings end up only at the named SDK.
func FuzzSDKFlags(f *testing.F) {
	f.Add("kv", "http:host=a.com", "kv:quota=1Mi")
	f.Add("./sdks/echo", "echo:ticks=3", "echo:x=env:Y")
	f.Add("kv", "kv:=", "::=")
	f.Fuzz(func(t *testing.T, sdk, grant, cfg string) {
		sf := sdkFlags{sdks: multi{sdk}, grants: multi{grant}, configs: multi{cfg}}
		uses, err := sf.uses()
		if err == nil && len(uses) != 1 {
			t.Fatalf("%d SDKs", len(uses))
		}
	})
}
