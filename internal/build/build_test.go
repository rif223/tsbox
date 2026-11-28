package build

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rif223/tsbox/internal/denobin"
)

func deno(t *testing.T) string {
	t.Helper()
	d, err := denobin.Find()
	if err != nil {
		t.Skip(err)
	}
	return d
}

func write(t *testing.T, dir, name, src string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBundle(t *testing.T) {
	d := deno(t)
	dir := t.TempDir()
	write(t, dir, "lib.ts", `export const greet = (s: string): string => "hi " + s;`)
	src := write(t, dir, "main.ts", `import { greet } from "./lib.ts"; import { script } from "@tsbox/core"; console.log(greet(script.name));`)
	a, err := Bundle(context.Background(), Options{Deno: d, StoreDir: filepath.Join(dir, "store")}, src)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(a.Path)
	// boot must come before the script code.
	if i, j := strings.Index(string(b), "transport.start()"), strings.Index(string(b), "greet("); i < 0 || j < 0 || i > j {
		t.Fatalf("boot not before the script (start=%d, greet=%d)", i, j)
	}
	if filepath.Base(a.Path) != a.SHA256+".js" {
		t.Fatalf("not content-addressed: %s", a.Path)
	}
	again, err := Bundle(context.Background(), Options{Deno: d, StoreDir: filepath.Join(dir, "store")}, src)
	if err != nil || again.SHA256 != a.SHA256 {
		t.Fatalf("build not reproducible: %v %s != %s", err, again.SHA256, a.SHA256)
	}
}

func TestBundleRejects(t *testing.T) {
	d := deno(t)
	dir := t.TempDir()
	cases := map[string]struct{ src, hint string }{
		"type error":  {`const n: number = "x"; console.log(n);`, "Type error"},
		"jsr":         {`import { assert } from "jsr:@std/assert@1"; assert(true);`, "Remote"},
		"npm":         {`import x from "npm:left-pad@1"; console.log(x);`, "Remote"},
		"https":       {`import x from "https://example.com/x.ts"; console.log(x);`, "Remote"},
		"SDK missing": {`import { kv } from "@tsbox/kv"; console.log(kv);`, "--sdk kv"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			src := write(t, dir, strings.ReplaceAll(name, " ", "_")+".ts", c.src)
			_, err := Bundle(context.Background(), Options{Deno: d, StoreDir: filepath.Join(dir, "store")}, src)
			var be *Error
			if !errors.As(err, &be) {
				t.Fatalf("expected a build error, got %v", err)
			}
			if !strings.Contains(be.Error(), c.hint) {
				t.Fatalf("hint %q missing:\n%s", c.hint, be.Error())
			}
		})
	}
}

// TestBundleIgnoresNestedConfig: a deno.json next to script and SDK (as in the repo or after tsbox types)
// that maps @tsbox/core to its own copy must not bring core into the bundle a second time and must not
// unlock further imports.
func TestBundleIgnoresNestedConfig(t *testing.T) {
	d := deno(t)
	dir := t.TempDir()
	for _, sub := range []string{"own", "sdk"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, dir, "own/mod.ts", `export const ownCore = "second copy";`)
	write(t, dir, "deno.json", `{"imports": {"@tsbox/core": "./own/mod.ts", "@tsbox/other": "./own/mod.ts"}}`)
	sdk := write(t, dir, "sdk/mod.ts", `import { script } from "@tsbox/core"; export const who = (): string => script.name;`)
	src := write(t, dir, "main.ts", `import { who } from "@tsbox/x"; console.log(who());`)
	o := Options{Deno: d, StoreDir: filepath.Join(dir, "store"), Imports: map[string]string{"@tsbox/x": sdk}}
	a, err := Bundle(context.Background(), o, src)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(a.Path)
	if n := strings.Count(string(b), "transport.ts\n"); n != 1 || strings.Contains(string(b), "second copy") {
		t.Fatalf("core bundled %d times (foreign deno.json used?)", n)
	}
	bad := write(t, dir, "bad.ts", `import { ownCore } from "@tsbox/other"; console.log(ownCore);`)
	var be *Error
	if _, err := Bundle(context.Background(), o, bad); !errors.As(err, &be) {
		t.Fatalf("import from a foreign deno.json built: %v", err)
	}
}

// In the build sandbox an import outside the script's directory gets its own hint instead of "Type error".
func TestHintOutsideSandbox(t *testing.T) {
	for _, d := range []string{
		`error: Module not found "file:///scripts/lib_outside.ts".` + "\n    at file:///src/outside.ts:2:23",
		"TS2307 [ERROR]: Cannot find module 'file:///secrets.json'.\n    at file:///src/evil.ts:1:15",
	} {
		if h := hint(d); !strings.Contains(h, "outside the script's directory") {
			t.Errorf("%q: hint %q", d, h)
		}
	}
	// A missing file inside the script's directory is not an outside import.
	if h := hint(`error: Module not found "file:///src/missing.ts".`); strings.Contains(h, "outside") {
		t.Errorf("missing file inside: %q", h)
	}
}
