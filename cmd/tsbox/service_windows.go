package main

import (
	"errors"

	winreg "golang.org/x/sys/windows/registry"
)

// runKey holds the commands Windows starts at the user's logon (no admin rights needed).
var runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

const runValue = "tsboxd"

func detectService() service {
	s := service{What: `logon entry HKCU\` + runKey + `\` + runValue, Start: "tsbox daemon start"}
	k, err := winreg.OpenKey(winreg.CURRENT_USER, runKey, winreg.QUERY_VALUE)
	if err != nil {
		return s
	}
	defer k.Close()
	_, _, err = k.GetStringValue(runValue)
	s.Installed = err == nil
	return s
}

// installService writes the logon entry; the caller starts the daemon.
func installService(exe string) error {
	k, _, err := winreg.CreateKey(winreg.CURRENT_USER, runKey, winreg.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(runValue, windowsRunValue(exe))
}

// removeService deletes the logon entry (the daemon itself is stopped by the caller).
func removeService() error {
	k, err := winreg.OpenKey(winreg.CURRENT_USER, runKey, winreg.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(runValue); err != nil && !errors.Is(err, winreg.ErrNotExist) {
		return err
	}
	return nil
}

// serviceManagesProcess: the logon entry only starts tsboxd; "tsbox daemon start/stop" control it.
const serviceManagesProcess = false
