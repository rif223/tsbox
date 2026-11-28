package build

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestBuildCache (host): hit for identical inputs; any change to the script directory, SDK module or
// toolchain rebuilds; imports outside the directory are cached with their hash; a tampered artifact does
// not count as a hit.
func TestBuildCache(t *testing.T) {
	d := deno(t)
	dir := t.TempDir()
	for _, sub := range []string{"app", "sdk", "shared", "app2"} {
		os.MkdirAll(filepath.Join(dir, sub), 0o755)
	}
	write(t, filepath.Join(dir, "sdk"), "mod.ts", `export const v = (): number => 1;`)
	write(t, filepath.Join(dir, "app"), "lib.ts", `export const greet = (s: string): string => "hi " + s;`)
	src := write(t, filepath.Join(dir, "app"), "main.ts", `import { greet } from "./lib.ts"; import { v } from "@x/sdk"; console.log(greet("a"), v());`)
	o := Options{Deno: d, StoreDir: filepath.Join(dir, "store"), Toolchain: "t1", Imports: map[string]string{"@x/sdk": filepath.Join(dir, "sdk", "mod.ts")}}
	build := func(o Options, src string) Artifact {
		t.Helper()
		a, err := Bundle(context.Background(), o, src)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	a1 := build(o, src)
	if a1.Cached {
		t.Fatal("first build from the cache")
	}
	if a2 := build(o, src); !a2.Cached || a2.SHA256 != a1.SHA256 {
		t.Fatalf("no hit: %+v", a2)
	}
	o2 := o
	o2.Toolchain = "t2"
	if a := build(o2, src); a.Cached {
		t.Fatal("different toolchain: hit")
	}
	if a := build(Options{Deno: d, StoreDir: o.StoreDir, Toolchain: "t1", Imports: o.Imports, NoCache: true}, src); a.Cached {
		t.Fatal("NoCache: hit")
	}
	write(t, filepath.Join(dir, "sdk"), "mod.ts", `export const v = (): number => 2;`)
	if a := build(o, src); a.Cached || a.SHA256 == a1.SHA256 {
		t.Fatalf("SDK changed: %+v", a)
	}
	write(t, filepath.Join(dir, "app"), "lib.ts", `export const greet = (s: string): string => "hello " + s;`)
	a3 := build(o, src)
	if a3.Cached {
		t.Fatal("script directory changed: hit")
	}
	// Type error in the directory: no hit, the build fails.
	write(t, filepath.Join(dir, "app"), "lib.ts", `export const greet = (s: string): number => "x" + s;`)
	if _, err := Bundle(context.Background(), o, src); err == nil {
		t.Fatal("type error after a cache hit not detected")
	}
	write(t, filepath.Join(dir, "app"), "lib.ts", `export const greet = (s: string): string => "hello " + s;`)
	if a := build(o, src); !a.Cached || a.SHA256 != a3.SHA256 {
		t.Fatalf("old state not hit again: %+v", a)
	}
	// Tampered artifact: rebuild.
	os.Chmod(a3.Path, 0o644)
	os.WriteFile(a3.Path, []byte("tampered"), 0o644)
	if a := build(o, src); a.Cached {
		t.Fatal("tampered artifact as a hit")
	}
	// Imports outside the script directory (only possible on the host), including pure type imports: cached,
	// but every change there rebuilds.
	write(t, filepath.Join(dir, "shared"), "util.ts", `export const u = 1;`)
	write(t, filepath.Join(dir, "shared"), "types.ts", `export type T = { a: number };`)
	src2 := write(t, filepath.Join(dir, "app2"), "main.ts",
		`import { u } from "../shared/util.ts"; import type { T } from "../shared/types.ts"; const x: T = { a: u }; console.log(x);`)
	b1 := build(o, src2)
	if a := build(o, src2); !a.Cached || a.SHA256 != b1.SHA256 {
		t.Fatalf("import outside the directory: no hit: %+v", a)
	}
	write(t, filepath.Join(dir, "shared"), "util.ts", `export const u = 2;`)
	if a := build(o, src2); a.Cached {
		t.Fatal("code outside changed: hit")
	}
	// A pure type import changes so that the build must fail.
	write(t, filepath.Join(dir, "shared"), "types.ts", `export type T = { a: string };`)
	if _, err := Bundle(context.Background(), o, src2); err == nil {
		t.Fatal("type import outside changed: type error not detected (cache hit)")
	}
}

func TestCacheLimits(t *testing.T) {
	dir := t.TempDir()
	src := write(t, dir, "main.ts", "console.log(1)")
	old := maxCacheFiles
	defer func() { maxCacheFiles = old }()
	maxCacheFiles = 1
	write(t, dir, "x.ts", "1")
	if _, _, err := cacheKey(Options{Toolchain: "t"}, dir, src); err == nil {
		t.Fatal("limit not enforced")
	}
}
