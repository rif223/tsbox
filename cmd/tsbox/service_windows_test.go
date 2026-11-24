package main

import (
	"fmt"
	"os"
	"testing"

	winreg "golang.org/x/sys/windows/registry"
)

// The logon entry is written, found and removed (in a test key, not the real Run key).
func TestWindowsLogonEntry(t *testing.T) {
	old := runKey
	runKey = fmt.Sprintf(`Software\tsbox-test-%d`, os.Getpid())
	t.Cleanup(func() {
		_ = winreg.DeleteKey(winreg.CURRENT_USER, runKey)
		runKey = old
	})
	if detectService().Installed {
		t.Fatal("found before install")
	}
	if err := installService(`C:\tsbox\tsbox.exe`); err != nil {
		t.Fatal(err)
	}
	s := detectService()
	if !s.Installed || s.Start != "tsbox daemon start" {
		t.Fatalf("%+v", s)
	}
	k, err := winreg.OpenKey(winreg.CURRENT_USER, runKey, winreg.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := k.GetStringValue(runValue)
	k.Close()
	if err != nil || v != `"C:\tsbox\tsbox.exe" daemon start` {
		t.Fatalf("%q %v", v, err)
	}
	if err := removeService(); err != nil {
		t.Fatal(err)
	}
	if detectService().Installed {
		t.Fatal("still found after remove")
	}
	if err := removeService(); err != nil {
		t.Fatalf("second remove: %v", err)
	}
}
