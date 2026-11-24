package main

import (
	"strings"
	"testing"
)

// A set-up service blocks the autostart only for the default state; tests and custom states keep it.
func TestServiceBlocksAutostart(t *testing.T) {
	t.Setenv("TSBOX_STATE", "")
	if !serviceBlocksAutostart(service{Installed: true}) {
		t.Error("set-up service does not block")
	}
	if serviceBlocksAutostart(service{}) {
		t.Error("no service, but blocked")
	}
	t.Setenv("TSBOX_STATE", t.TempDir())
	if serviceBlocksAutostart(service{Installed: true}) {
		t.Error("custom state blocked")
	}
	err := notRunningError(service{What: "systemd unit x", Start: "sudo systemctl start tsboxd"}, " (hint)")
	if !strings.Contains(err.Error(), "systemctl start tsboxd") || !strings.Contains(err.Error(), "hint") {
		t.Errorf("message: %v", err)
	}
}

func TestLaunchdPlist(t *testing.T) {
	p := launchdPlist("/Users/a&b/bin/tsbox", "/Users/a&b/state/tsboxd.log")
	for _, want := range []string{"<string>" + launchdLabel + "</string>", "<string>/Users/a&amp;b/bin/tsbox</string>",
		"<string>daemon</string><string>--idle</string><string>0</string>", "<key>SuccessfulExit</key><false/>",
		"<key>RunAtLoad</key><true/>"} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %s in\n%s", want, p)
		}
	}
	if strings.Contains(p, "a&b") {
		t.Error("& not escaped")
	}
}

func TestWindowsRunValue(t *testing.T) {
	if v := windowsRunValue(`C:\Program Files\tsbox\tsbox.exe`); v != `"C:\Program Files\tsbox\tsbox.exe" daemon start` {
		t.Errorf("%s", v)
	}
	for exe, tmp := range map[string]bool{
		`C:\Users\a\AppData\Local\Temp\go-build123\b001\exe\tsbox.exe`: true,
		`C:\Users\a\Downloads\tsbox_0.1.0_windows_amd64\tsbox.exe`:     true,
		`/tmp/tsbox`:                       true,
		`C:\Program Files\tsbox\tsbox.exe`: false,
		`/usr/local/bin/tsbox`:             false,
	} {
		if temporaryExe(exe) != tmp {
			t.Errorf("%s: temporary=%v", exe, !tmp)
		}
	}
}
