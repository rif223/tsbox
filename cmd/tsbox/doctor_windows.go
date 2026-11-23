package main

import (
	"os"
	"path/filepath"
	"strings"
)

// virtualizedState reports whether the state directory is subject to the file system virtualization of
// packaged (MSIX) apps: writes below %LOCALAPPDATA% and %APPDATA% of a process started from such an app (for
// example from its built-in terminal) land in %LOCALAPPDATA%\Packages\<family>\LocalCache\{Local,Roaming}.
// That breaks os.RemoveAll (tsbox falls back, see fsutil.RemoveAll), slows builds down, and tsbox started
// outside the app sees a different state. The check looks at the effect, not at the package identity:
// processes of such an app may lack the identity (GetCurrentPackageFamilyName fails) and still be
// redirected. It also reports copies of the state that packaged apps left behind. "" = nothing to report.
func virtualizedState(state string) string {
	local := os.Getenv("LOCALAPPDATA")
	rel, sub := appDataRel(state, local, os.Getenv("APPDATA"))
	if rel == "" {
		return ""
	}
	pattern := filepath.Join(local, "Packages", "*", "LocalCache", sub, rel)
	if f, err := os.CreateTemp(state, ".doctor-virt-*"); err == nil {
		name := filepath.Base(f.Name())
		f.Close()
		hits, _ := filepath.Glob(filepath.Join(pattern, name))
		os.Remove(f.Name())
		if len(hits) > 0 {
			return "Windows redirects writes to " + state + " into " + filepath.Dir(hits[0]) +
				" (tsbox runs inside a packaged app, e.g. its terminal): builds are slower and tsbox outside the" +
				" app sees a different state. Set TSBOX_STATE to a directory outside %LOCALAPPDATA% and %APPDATA%"
		}
	}
	if copies, _ := filepath.Glob(pattern); len(copies) > 0 {
		return "packaged apps keep their own copy of the state (" + strings.Join(copies, ", ") +
			"); boxes created from inside such an app are not visible here"
	}
	return ""
}

// appDataRel returns state relative to %LOCALAPPDATA% ("Local") or %APPDATA% ("Roaming"), or "" when state
// is outside both or below %LOCALAPPDATA%\Temp and \Packages (never virtualized).
func appDataRel(state, local, roaming string) (rel, sub string) {
	for _, b := range []struct{ dir, sub string }{{local, "Local"}, {roaming, "Roaming"}} {
		if b.dir == "" {
			continue
		}
		r, err := filepath.Rel(b.dir, state)
		if err != nil || r == "." || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) || filepath.IsAbs(r) {
			continue
		}
		first, _, _ := strings.Cut(r, string(filepath.Separator))
		if b.sub == "Local" && (strings.EqualFold(first, "Temp") || strings.EqualFold(first, "Packages")) {
			return "", ""
		}
		return r, b.sub
	}
	return "", ""
}
