package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/rif223/tsbox"
)

func plistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
}

func launchdTarget() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

func detectService() service {
	s := service{What: "LaunchAgent " + plistPath(), Start: "launchctl kickstart " + launchdTarget() + "/" + launchdLabel}
	_, err := os.Stat(plistPath())
	s.Installed = err == nil
	return s
}

// installService writes the LaunchAgent and loads it; launchd starts tsboxd.
func installService(exe string) error {
	p := plistPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	log := filepath.Join(tsbox.DefaultStateDir(), "tsboxd.log")
	if err := os.MkdirAll(filepath.Dir(log), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(launchdPlist(exe, log)), 0o644); err != nil {
		return err
	}
	_ = exec.Command("launchctl", "bootout", launchdTarget(), p).Run() // an older version of the agent
	if out, err := exec.Command("launchctl", "bootstrap", launchdTarget(), p).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, out)
	}
	return nil
}

// removeService unloads the LaunchAgent (launchd stops tsboxd) and deletes it.
func removeService() error {
	p := plistPath()
	_ = exec.Command("launchctl", "bootout", launchdTarget(), p).Run()
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// serviceManagesProcess: launchd starts tsboxd itself.
const serviceManagesProcess = true
