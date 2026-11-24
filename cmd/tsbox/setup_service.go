package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/rif223/tsbox"
	"github.com/rif223/tsbox/client"
	"github.com/rif223/tsbox/internal/daemon"
)

// userDaemon connects to the running user daemon of the default state (nil if none answers).
func userDaemon(ctx context.Context) (*client.Client, client.Info) {
	c, err := client.New(client.UserHost(tsbox.DefaultStateDir()))
	if err != nil {
		return nil, client.Info{}
	}
	info, err := ping(ctx, c)
	if err != nil {
		c.Close()
		return nil, client.Info{}
	}
	return c, info
}

// stopUserDaemon ends the running user daemon unless boxes run in it. It reports whether no daemon runs
// afterwards.
func stopUserDaemon(ctx context.Context) bool {
	c, info := userDaemon(ctx)
	if c == nil {
		return true
	}
	defer c.Close()
	if info.Running > 0 {
		fmt.Printf("   tsboxd (PID %d) keeps running: %d box(es) run in it; \"tsbox daemon stop\" later\n", info.PID, info.Running)
		return false
	}
	_ = c.Shutdown(ctx)
	waitGone(ctx, c)
	fmt.Printf("== stopped tsboxd (PID %d)\n", info.PID)
	return true
}

// setupUserService sets tsboxd up to run permanently for this user (Windows: logon entry, macOS:
// LaunchAgent) and starts it, so it runs like Docker Desktop's engine instead of on demand.
func setupUserService(dry bool) int {
	fail := func(err error) int {
		fmt.Fprintln(os.Stderr, "tsbox setup:", err)
		return 1
	}
	exe, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return fail(err)
	}
	s := detectService()
	fmt.Printf("== tsbox uses the dev driver on %s (NO isolation)\n", runtime.GOOS)
	fmt.Printf("== start tsboxd at logon: %s (%s)\n", s.What, exe)
	if temporaryExe(exe) {
		fmt.Printf("   note: %s looks temporary; put tsbox where it stays and run \"tsbox setup\" from there\n", exe)
	}
	if dry {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	// A daemon of another build, or an autostarted one (it would end when idle), is replaced if no box runs.
	if c, info := userDaemon(ctx); c != nil {
		c.Close()
		if serviceManagesProcess || info.Autostarted || info.Build != daemon.BuildID() {
			if !stopUserDaemon(ctx) && serviceManagesProcess {
				return fail(fmt.Errorf("stop the boxes first, then run \"tsbox setup\" again"))
			}
		}
	}
	if err := installService(exe); err != nil {
		return fail(err)
	}
	if serviceManagesProcess { // launchd starts it
		deadline := time.Now().Add(30 * time.Second)
		for {
			if c, info := userDaemon(ctx); c != nil {
				c.Close()
				fmt.Printf("== tsboxd runs (PID %d)\n", info.PID)
				break
			}
			if time.Now().After(deadline) {
				return fail(fmt.Errorf("tsboxd did not start; see %s", filepath.Join(tsbox.DefaultStateDir(), "tsboxd.log")))
			}
			time.Sleep(200 * time.Millisecond)
		}
	} else if c, info := userDaemon(ctx); c != nil {
		c.Close()
		fmt.Printf("== tsboxd runs (PID %d)\n", info.PID)
	} else {
		c, info, err := startDaemon(ctx, client.UserHost(tsbox.DefaultStateDir()), true)
		if err != nil {
			return fail(err)
		}
		c.Close()
		fmt.Printf("== started tsboxd (PID %d)\n", info.PID)
	}
	fmt.Println()
	return doctorCmd(nil)
}

// setupRemove undoes the service part of "tsbox setup": tsboxd no longer starts by itself. Boxes, rootfs and
// the group stay; without a service the CLI starts tsboxd on demand again.
func setupRemove(dry bool) int {
	s := detectService()
	if !s.Installed {
		fmt.Println("== nothing to remove: tsboxd is not set up as a service")
		return 0
	}
	fmt.Println("== remove", s.What)
	if dry {
		return 0
	}
	if runtime.GOOS == "linux" && os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "tsbox setup: please run as root (sudo)")
		return 1
	}
	if err := removeService(); err != nil {
		fmt.Fprintln(os.Stderr, "tsbox setup:", err)
		return 1
	}
	if !serviceManagesProcess {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		stopUserDaemon(ctx)
	}
	fmt.Println("== done; boxes stay. Without a service tsbox starts tsboxd on demand again.")
	return 0
}
