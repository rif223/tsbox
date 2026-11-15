package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/rif223/tsbox"
	"github.com/rif223/tsbox/internal/denobin"
	"github.com/rif223/tsbox/internal/fsutil"
	"github.com/rif223/tsbox/internal/providers/extproc"
	"github.com/rif223/tsbox/internal/sdkreg"
	"github.com/rif223/tsbox/internal/sdktest"
	"github.com/rif223/tsbox/js"
	"github.com/rif223/tsbox/manifest"
	"github.com/rif223/tsbox/sdk"
)

func registry() *sdkreg.Registry { return sdkreg.New(tsbox.DefaultStateDir()) }

// check: tsbox check <script.ts> [--sdk …] – build with type checking and complete SDK checks.
func check(args []string) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	var sf sdkFlags
	sf.register(fs)
	usageFor(fs, "check <script.ts> [--sdk ...] [--grant ...]")
	entry, ok := parseArgs(fs, args, true)
	if !ok {
		return 2
	}
	uses, err := sf.uses()
	if err != nil {
		return usagef("%v", err)
	}
	if st, err := os.Stat(entry); err == nil && st.IsDir() {
		return usagef("%s is a directory, not a script", entry)
	} else if err != nil {
		return usagef("%s", notFound(entry))
	}
	ctx := context.Background()
	e, err := tsbox.New(ctx, tsbox.WithDriver("dev"))
	if err != nil {
		return failf("%v", err)
	}
	defer e.Close(ctx)
	hashes, err := e.Check(ctx, tsbox.BoxSpec{Name: "check", SDKs: uses,
		Scripts: []tsbox.ScriptSpec{{Name: "main", Entry: entry}}})
	if err != nil {
		printBuildErr(err)
		return 1
	}
	fmt.Printf("ok  %s  sha256:%s\n", entry, hashes["main"])
	return 0
}

// types: writes .tsbox/types/<sdk>/ and adds the imports to deno.json so that the editor (VS Code with the
// Deno extension) has types and auto-completion. The build ignores this file.
func types(args []string) int {
	fs := flag.NewFlagSet("types", flag.ContinueOnError)
	dir := fs.String("dir", ".", "project directory")
	var sf sdkFlags
	fs.Var(&sf.sdks, "sdk", "SDK whose types to add (name or path; repeatable)")
	usageFor(fs, "types [--dir .] [--sdk ...]")
	if _, ok := parsePos(fs, args, 0, 0); !ok {
		return 2
	}
	if st, err := os.Stat(*dir); err != nil || !st.IsDir() {
		return usagef("--dir %s: not an existing directory", *dir)
	}
	reg := registry()
	out := filepath.Join(*dir, ".tsbox", "types")
	_ = os.RemoveAll(out)
	imports := map[string]string{
		"@tsbox/core": "./.tsbox/types/core/mod.ts",
	}
	if err := writeEmbedded(js.Core, "core", filepath.Join(out, "core")); err != nil {
		return failf("%v", err)
	}
	for _, ref := range sf.sdks {
		s, err := reg.Resolve(ref)
		if err != nil {
			return failf("%v", err)
		}
		dst := filepath.Join(out, s.Manifest.Name)
		if err := copyTS(s.Dir, dst); err != nil {
			return failf("%v", err)
		}
		imports[s.Manifest.Import] = "./.tsbox/types/" + s.Manifest.Name + "/" + filepath.ToSlash(s.Manifest.Module)
	}
	cfgPath := filepath.Join(*dir, "deno.json")
	cfg := map[string]any{}
	if b, err := fsutil.ReadText(cfgPath); errors.Is(err, fsutil.ErrNotUTF8) {
		return failf("%s is %v", cfgPath, err)
	} else if err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			if bytes.Contains(b, []byte("//")) || bytes.Contains(b, []byte("/*")) {
				return failf("%s is not plain JSON (tsbox cannot update files with comments): %v", cfgPath, err)
			}
			return failf("%s is not valid JSON: %v", cfgPath, err)
		}
	}
	existing, _ := cfg["imports"].(map[string]any)
	if existing == nil {
		existing = map[string]any{}
	}
	var dropped []string
	for k, v := range existing {
		if strings.HasPrefix(fmt.Sprint(v), "./.tsbox/types/") {
			delete(existing, k) // replace old tsbox entries
			if _, again := imports[k]; !again {
				dropped = append(dropped, k)
			}
		}
	}
	if len(dropped) > 0 {
		sort.Strings(dropped)
		fmt.Fprintf(os.Stderr, "tsbox: removed from deno.json (not given with --sdk): %s\n", strings.Join(dropped, ", "))
	}
	for k, v := range imports {
		if old, ok := existing[k]; ok {
			// An entry of the project itself (e.g. a local path): keep it, the editor then uses that one.
			fmt.Fprintf(os.Stderr, "tsbox: %s already maps %s to %v; left unchanged\n", cfgPath, k, old)
			delete(imports, k)
			continue
		}
		existing[k] = v
	}
	cfg["imports"] = existing
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(cfgPath, append(b, '\n'), 0o644); err != nil {
		return failf("%v", err)
	}
	keys := make([]string, 0, len(imports))
	for k := range imports {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("Types for %s written to %s; %s updated.\n", strings.Join(keys, ", "), out, cfgPath)
	fmt.Println("Tip: add .tsbox/ to .gitignore.")
	return 0
}

// failf reports a failed sdk, check or types command (exit 1).
func failf(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "tsbox: "+format+"\n", a...)
	return 1
}

// usagef reports wrong input (exit 2, as for unknown flags).
func usagef(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "tsbox: "+format+"\n", a...)
	return 2
}

func writeEmbedded(fsys fs.FS, root, dst string) error {
	return fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.FromSlash(strings.TrimPrefix(p, root+"/")))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}

// copyTS copies the TS files of an SDK (without provider code) for the editor.
func copyTS(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			if rel == "provider" || d.Name() == "node_modules" || d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".ts") && d.Name() != manifest.FileName || d.Type()&(fs.ModeNamedPipe|fs.ModeSocket|fs.ModeDevice|fs.ModeCharDevice|fs.ModeIrregular) != 0 {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}

func sdkCmd(args []string) int {
	const help = `Usage: tsbox sdk <command>
  ls                         bundled and installed SDKs
  info <name|path>           manifest: methods, rights, settings, events
  add <path|git+url#ref>     install an SDK (hash in lock.json)
  rm <name>                  remove an SDK
  init <name> [--provider deno|go] [--dir .]
                             create a new SDK
  test <dir> [--grant ...]   check manifest, types, provider conformance and example.ts
`
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, help)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Print(help)
		return 0
	case "init":
		return sdkInit(args[1:])
	case "test":
		return sdkTest(args[1:])
	}
	sub := args[0]
	fs := flag.NewFlagSet("sdk "+sub, flag.ContinueOnError)
	var pos []string
	var ok bool
	switch sub {
	case "ls":
		usageFor(fs, "sdk ls")
		pos, ok = parsePos(fs, args[1:], 0, 0)
	case "info":
		usageFor(fs, "sdk info <name|path>")
		pos, ok = parsePos(fs, args[1:], 1, 1)
	case "add":
		usageFor(fs, "sdk add <path|git+https://...#ref>")
		pos, ok = parsePos(fs, args[1:], 1, 1)
	case "rm":
		usageFor(fs, "sdk rm <name>")
		pos, ok = parsePos(fs, args[1:], 1, 1)
	default:
		msg := fmt.Sprintf("tsbox sdk: unknown command %q", sub)
		if sug := closest(sub, []string{"ls", "info", "add", "rm", "init", "test"}); sug != "" {
			msg += fmt.Sprintf(" (did you mean %q?)", sug)
		}
		fmt.Fprint(os.Stderr, msg+"\n"+help)
		return 2
	}
	if !ok {
		return 2
	}
	reg := registry()
	switch sub {
	case "ls":
		list, err := reg.List()
		if err != nil {
			return failf("%v", err)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tVERSION\tIMPORT\tPROVIDER\tSOURCE")
		for _, s := range list {
			prov := "builtin"
			if len(s.Manifest.Provider.Exec) > 0 {
				prov = "external"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.Manifest.Name, s.Manifest.Version, s.Manifest.Import, prov, s.Source)
		}
		w.Flush()
	case "info":
		s, err := reg.Resolve(pos[0])
		if err != nil {
			return failf("%v", err)
		}
		printInfo(s)
	case "add":
		if m, err := manifest.Load(pos[0]); err == nil {
			if err := releaseSDK(m.Name); err != nil {
				return failf("%v", err)
			}
		}
		s, err := reg.Add(pos[0])
		if err != nil {
			return failf("%v", err)
		}
		fmt.Printf("installed: %s %s (%s), sha256:%s\n", s.Manifest.Name, s.Manifest.Version, s.Manifest.Import, s.Hash[:12])
	case "rm":
		if err := releaseSDK(pos[0]); err != nil {
			return failf("%v", err)
		}
		if err := reg.Remove(pos[0]); err != nil {
			return failf("%v", err)
		}
		fmt.Println("removed:", pos[0])
	}
	return 0
}

func printInfo(s *sdkreg.SDK) {
	m := s.Manifest
	fmt.Printf("%s %s - %s\nImport: %s   Source: %s   sha256:%s\n", m.Name, m.Version, m.Description, m.Import, s.Source, s.Hash[:12])
	if m.Provider.Builtin != "" {
		fmt.Printf("Provider: built-in (%s)\n", m.Provider.Builtin)
	} else {
		fmt.Printf("Provider: external %v\n", m.Provider.Exec)
	}
	section := func(title string, keys []string, line func(string) string) {
		if len(keys) == 0 {
			return
		}
		sort.Strings(keys)
		fmt.Println("\n" + title)
		for _, k := range keys {
			fmt.Println("  " + line(k))
		}
	}
	var mk, pk, ck, ek []string
	for k := range m.Methods {
		mk = append(mk, k)
	}
	for k := range m.Permissions {
		pk = append(pk, k)
	}
	for k := range m.Config {
		ck = append(ck, k)
	}
	for k := range m.Events {
		ek = append(ek, k)
	}
	section("Methods:", mk, func(k string) string {
		x := m.Methods[k]
		perm := ""
		if x.Permission != "" {
			perm = "  right: " + x.Permission
		}
		return fmt.Sprintf("%-10s %g/s (burst %d), %d concurrent, parameters <= %s, result <= %s, %v%s",
			k, x.Rate, x.Burst, x.Concurrency, manifest.FormatBytes(x.MaxParams), manifest.FormatBytes(x.MaxResult), x.Timeout, perm)
	})
	section("Rights (--grant "+m.Name+":<right>=<value>):", pk, func(k string) string {
		p := m.Permissions[k]
		flags := p.Type
		if p.Required {
			flags += ", required"
		}
		if p.Multiple {
			flags += ", repeatable"
		}
		return fmt.Sprintf("%-10s (%s) %s", k, flags, p.Description)
	})
	section("Settings (--sdk-config "+m.Name+":<name>=<value>):", ck, func(k string) string {
		c := m.Config[k]
		def := ""
		if c.Default != "" {
			def = " = " + c.Default
		}
		return fmt.Sprintf("%-12s (%s%s) %s", k, c.Type, def, c.Description)
	})
	section("Events:", ek, func(k string) string { return k + "  " + m.Events[k].Description })
}

// sdkTest checks an SDK directory: manifest, types, provider conformance and – if present – example.ts in a
// real box (dev driver).
func sdkTest(args []string) int {
	fs := flag.NewFlagSet("sdk test", flag.ContinueOnError)
	var sf sdkFlags
	sf.register(fs)
	usageFor(fs, "sdk test <dir> [--grant ...] [--sdk-config ...]")
	dir, ok := parseArgs(fs, args, true)
	if !ok {
		return 2
	}
	step := func(name string, err error) bool {
		if err != nil {
			fmt.Printf("ERROR   %s: %v\n", name, err)
			return false
		}
		fmt.Printf("ok      %s\n", name)
		return true
	}
	m, err := manifest.Load(dir)
	if !step("manifest", err) {
		return 1
	}
	deno, err := denobin.Find()
	if !step("Deno", err) {
		return 1
	}
	// Type check of the module with @tsbox/core and required SDKs.
	tmp, _ := os.MkdirTemp("", "tsbox-sdktest-")
	defer os.RemoveAll(tmp)
	_ = writeEmbedded(js.Core, "core", filepath.Join(tmp, "core"))
	abs, _ := filepath.Abs(dir)
	imports := map[string]string{"@tsbox/core": fileURL(filepath.Join(tmp, "core", "mod.ts")), m.Import: fileURL(filepath.Join(abs, m.Module))}
	for _, req := range m.Requires {
		if s, err := registry().Resolve(req); err == nil {
			imports[s.Manifest.Import] = fileURL(s.ModulePath())
		}
	}
	cfg, _ := json.Marshal(map[string]any{"imports": imports})
	// --no-config: a deno.json next to the SDK (repo, tsbox types) must not extend the import map.
	_ = os.WriteFile(filepath.Join(tmp, "import_map.json"), cfg, 0o600)
	out, err := exec.Command(deno, "check", "--quiet", "--no-config", "--import-map", filepath.Join(tmp, "import_map.json"),
		filepath.Join(abs, m.Module)).CombinedOutput()
	if err != nil {
		err = fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	if !step("types ("+m.Module+")", err) {
		return 1
	}
	if len(m.Provider.Exec) > 0 {
		if !step("provider conformance", conformance(abs, m, deno)) {
			return 1
		}
	} else {
		fmt.Printf("-       provider conformance: built-in provider %q (checked in the engine)\n", m.Provider.Builtin)
	}
	example := filepath.Join(abs, "example.ts")
	if _, err := os.Stat(example); err != nil {
		fmt.Println("-       example.ts missing (optional)")
		return 0
	}
	sf.sdks = append([]string{abs}, sf.sdks...)
	uses, err := sf.uses()
	if !step("grants", err) {
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	e, err := tsbox.New(ctx, tsbox.WithDriver("dev"), tsbox.WithLogs(func(l tsbox.LogLine) { fmt.Println("        |", l.Msg) }))
	if !step("engine", err) {
		return 1
	}
	defer e.Close(context.Background())
	b, err := e.Run(ctx, tsbox.BoxSpec{Name: "sdk-test-" + m.Name, SDKs: uses, Storage: "sdk-test",
		Scripts: []tsbox.ScriptSpec{{Name: "example", Entry: example, Limits: tsbox.ScriptLimits{Timeout: time.Minute}}}})
	if err != nil {
		printBuildErr(err)
		return 1
	}
	st, err := b.Wait(ctx)
	if err == nil {
		if ex := st.Scripts[0].LastExit; ex == nil || ex.Reason != tsbox.ReasonExit {
			err = fmt.Errorf("example.ts ended with %+v", ex)
		}
	}
	if !step("example.ts in a box", err) {
		return 1
	}
	return 0
}

// conformance starts the external provider and checks handshake, open (with placeholders for required
// rights), unknown method ⇒ not_found, and close.
func conformance(dir string, m *manifest.Manifest, deno string) error {
	settings, err := m.ResolveConfig(nil)
	if err != nil {
		return fmt.Errorf("%v (set the required settings for the test via the environment)", err)
	}
	p := extproc.New(extproc.Config{Name: m.Name, Version: m.Version, Dir: dir, Exec: m.Provider.Exec, Deno: deno, Settings: settings})
	defer p.Close()
	grants := sdk.Grants{}
	for k, perm := range m.Permissions {
		if perm.Required {
			switch perm.Type {
			case "host-pattern":
				grants[k] = []string{"example.com"}
			case "int", "bytes":
				grants[k] = []string{"1"}
			case "enum":
				grants[k] = []string{perm.Values[0]}
			default:
				grants[k] = []string{"test"}
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h, err := p.Open(ctx, &sdktest.Session{BoxRef: sdk.BoxRef{Name: "conformance", Storage: "conformance"},
		ScriptRef: sdk.ScriptRef{Name: "conformance"}, G: grants})
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	_, err = h.Call(ctx, "__tsbox_conformance_unknown__", json.RawMessage(`{}`))
	var se *sdk.Error
	if !errorsAs(err, &se) || se.Code != sdk.NotFound {
		return fmt.Errorf("unknown method must return not_found, got %v", err)
	}
	return h.Close()
}

func errorsAs(err error, target **sdk.Error) bool {
	for err != nil {
		if se, ok := err.(*sdk.Error); ok {
			*target = se
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func fileURL(p string) string {
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return "file://" + p
}
