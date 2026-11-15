package build

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Paths in the build sandbox. They are fixed so that identical sources give identical artifacts.
const (
	boxWork = "/work"
	boxCore = "/core"
	boxSrc  = "/src"
	boxSDK  = "/sdk"
)

// bundleSandbox builds src via o.Runner: sources (Context), @tsbox/core and SDK modules read-only, entry and
// import map in /work (without deno.json, see the package doc), output via stdout.
func bundleSandbox(ctx context.Context, o Options, src, coreDir string) (Artifact, error) {
	ctxDir := o.Context
	if ctxDir == "" {
		ctxDir = filepath.Dir(src)
	}
	ctxDir, err := filepath.Abs(ctxDir)
	if err != nil {
		return Artifact{}, err
	}
	rel, err := filepath.Rel(ctxDir, src)
	if err != nil || strings.HasPrefix(rel, "..") {
		return Artifact{}, fmt.Errorf("script %s is outside the build context %s", src, ctxDir)
	}
	work, err := os.MkdirTemp("", "tsbox-sbuild-")
	if err != nil {
		return Artifact{}, err
	}
	defer os.RemoveAll(work)
	_ = os.Chmod(work, 0o755)

	mounts := []Mount{{Host: work, Box: boxWork}, {Host: coreDir, Box: boxCore}, {Host: ctxDir, Box: boxSrc}}
	imports := map[string]string{
		"@tsbox/core":      "file://" + boxCore + "/mod.ts",
		"@tsbox/core/boot": "file://" + boxCore + "/boot.ts",
	}
	names := make([]string, 0, len(o.Imports))
	for name := range o.Imports {
		names = append(names, name)
	}
	sort.Strings(names)
	for i, name := range names {
		mod, err := filepath.Abs(o.Imports[name])
		if err != nil {
			return Artifact{}, err
		}
		dir := path.Join(boxSDK, fmt.Sprint(i))
		mounts = append(mounts, Mount{Host: filepath.Dir(mod), Box: dir})
		imports[name] = "file://" + path.Join(dir, filepath.Base(mod))
	}
	cfg, _ := json.Marshal(map[string]any{"imports": imports})
	entrySrc := fmt.Sprintf("import \"@tsbox/core/boot\";\nimport %q;\n", "file://"+path.Join(boxSrc, filepath.ToSlash(rel)))
	if err := os.WriteFile(filepath.Join(work, "import_map.json"), cfg, 0o644); err != nil {
		return Artifact{}, err
	}
	if err := os.WriteFile(filepath.Join(work, "entry.ts"), []byte(entrySrc), 0o644); err != nil {
		return Artifact{}, err
	}
	out, diag, err := o.Runner(ctx, Invocation{Mounts: mounts, Args: []string{
		"bundle", "--quiet", "--no-config", "--import-map", boxWork + "/import_map.json", "--no-lock", "--no-remote",
		"--no-npm", "--check", "--platform", "deno", "--sourcemap=inline", boxWork + "/entry.ts",
	}})
	if err != nil {
		text := ansi.ReplaceAllString(string(diag), "")
		if strings.TrimSpace(text) == "" {
			return Artifact{}, fmt.Errorf("build sandbox: %w", err)
		}
		text = strings.ReplaceAll(text, "file://"+boxSrc+"/", "")
		return Artifact{}, &Error{Output: text, Hint: hint(text)}
	}
	if len(out) == 0 {
		return Artifact{}, fmt.Errorf("build sandbox returned no bundle: %s", diag)
	}
	// Sources are relative to the working directory boxWork: box paths such as file:///src/main.ts.
	out = fixSourceMap(out, func(s string) string {
		if strings.Contains(s, "://") {
			return s
		}
		return "file://" + path.Join(boxWork, s)
	})
	return Store(o.StoreDir, out)
}
