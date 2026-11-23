package build

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/rif223/tsbox/internal/fsutil"
)

// WarmCache fills cacheDir once as the read-only DENO_DIR of a build sandbox (needs network): a bundle with
// deno loads esbuild (spike e). Afterwards all files are world-readable, because the sandbox builds as
// UID 65534.
func WarmCache(ctx context.Context, deno, cacheDir string) error {
	if _, err := os.Stat(filepath.Join(cacheDir, ".tsbox-ready")); err == nil {
		return nil
	}
	tmp, err := os.MkdirTemp("", "tsbox-warm-")
	if err != nil {
		return err
	}
	defer fsutil.RemoveAll(tmp)
	src := filepath.Join(tmp, "warm.ts")
	if err := os.WriteFile(src, []byte("export const x: number = 1;\n"), 0o644); err != nil {
		return err
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, deno, "bundle", "--quiet", "--no-config", "--check", "--platform", "deno",
		"-o", filepath.Join(tmp, "out.js"), src)
	cmd.Env = []string{"DENO_DIR=" + cacheDir, "HOME=" + tmp, "NO_COLOR=1", "PATH=/usr/bin:/bin"}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("filling the build cache (loads esbuild once, needs network): %v\n%s", err, out)
	}
	_ = filepath.Walk(cacheDir, func(p string, info os.FileInfo, err error) error {
		if err == nil {
			mode := os.FileMode(0o644)
			if info.IsDir() || info.Mode()&0o111 != 0 {
				mode = 0o755
			}
			_ = os.Chmod(p, mode)
		}
		return nil
	})
	return os.WriteFile(filepath.Join(cacheDir, ".tsbox-ready"), nil, 0o644)
}
