package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// unitPath is the systemd unit of the system daemon ("tsbox setup").
var unitPath = "/etc/systemd/system/tsboxd.service"

func detectService() service {
	s := service{What: "systemd unit " + unitPath, Start: "sudo systemctl start tsboxd"}
	_, err := os.Stat(unitPath)
	s.Installed = err == nil
	return s
}

// installService: on Linux setupCmd writes the systemd unit itself (root, group, environment).
func installService(string) error { return errors.New("use sudo tsbox setup") }

// removeService stops and disables the system daemon and deletes its unit.
func removeService() error {
	if _, err := os.Stat(unitPath); err != nil {
		return nil
	}
	if out, err := exec.Command("systemctl", "disable", "--now", "tsboxd").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl disable --now tsboxd: %v: %s", err, out)
	}
	if err := os.Remove(unitPath); err != nil {
		return err
	}
	return exec.Command("systemctl", "daemon-reload").Run()
}

// serviceManagesProcess: systemd starts and stops tsboxd.
const serviceManagesProcess = true
