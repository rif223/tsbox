package main

import (
	"fmt"
	"os"
	"strings"
)

// service describes how tsboxd is set up to run permanently ("tsbox setup"): systemd on Linux, an entry
// that starts it at logon on Windows, a LaunchAgent on macOS.
type service struct {
	Installed bool
	What      string // e.g. "systemd unit /etc/systemd/system/tsboxd.service"
	Start     string // command that starts it
}

// serviceBlocksAutostart reports the set-up service when the CLI must not start a daemon of its own: the
// service is the daemon of the default state, a second one would hold boxes nobody looks for.
func serviceBlocksAutostart(s service) bool {
	return s.Installed && os.Getenv("TSBOX_STATE") == ""
}

// notRunningError explains that the set-up service is not running.
func notRunningError(s service, hint string) error {
	return fmt.Errorf("tsboxd is set up (%s) but not running%s. Start it: %s", s.What, hint, s.Start)
}

// launchdLabel names the LaunchAgent on macOS.
const launchdLabel = "dev.tsbox.tsboxd"

// launchdPlist is the LaunchAgent: tsboxd at login without idle exit, restarted only after a crash (so that
// "tsbox daemon stop" stops it).
func launchdPlist(exe, logPath string) string {
	esc := func(s string) string {
		return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array><string>%s</string><string>daemon</string><string>--idle</string><string>0</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, launchdLabel, esc(exe), esc(logPath), esc(logPath))
}

// windowsRunValue is the command of the logon entry on Windows: "daemon start" starts tsboxd without a window
// and returns, so the console that Windows opens for it closes right away.
func windowsRunValue(exe string) string { return `"` + exe + `" daemon start` }

// temporaryExe reports whether exe looks like a copy that will go away (go run, downloads, temp).
func temporaryExe(exe string) bool {
	l := strings.ToLower(strings.ReplaceAll(exe, `\`, "/"))
	for _, p := range []string{"/go-build", "/tmp/", "/temp/", "/downloads/"} {
		if strings.Contains(l, p) {
			return true
		}
	}
	return false
}
