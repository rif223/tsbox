package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/rif223/tsbox"
	"github.com/rif223/tsbox/client"
	"github.com/rif223/tsbox/internal/daemon"
	"github.com/rif223/tsbox/internal/denobin"
	"github.com/rif223/tsbox/internal/driver/firecracker"
	"github.com/rif223/tsbox/internal/driver/gvisor"
	"github.com/rif223/tsbox/internal/profile"
	"github.com/rif223/tsbox/scripts"
)

type checker struct{ fails, warns int }

func (c *checker) ok(name, msg string)   { fmt.Printf("ok   %-16s %s\n", name, msg) }
func (c *checker) warn(name, msg string) { c.warns++; fmt.Printf("warn %-16s %s\n", name, msg) }
func (c *checker) fail(name, msg string) { c.fails++; fmt.Printf("FAIL %-16s %s\n", name, msg) }

func rootfsDir() string { return envOr("TSBOX_ROOTFS", "/var/lib/tsbox/rootfs") }

// profileSet: profiles as in tsboxd (TSBOX_ROOTFS, TSBOX_PROFILES).
func profileSet() profile.Set {
	return profile.Set{RootFS: rootfsDir(), Dir: envOr("TSBOX_PROFILES", "/var/lib/tsbox/profiles")}
}

func firstLine(b []byte) string {
	s, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(s)
}

// doctor checks the environment and reports whether isolation is available.
func doctorCmd(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	usageFor(fs, "doctor")
	if _, ok := parsePos(fs, args, 0, 0); !ok {
		return 2
	}
	var c checker
	root := runtime.GOOS != "windows" && os.Geteuid() == 0
	fmt.Printf("tsbox %s, %s/%s, root: %v\n\n", tsbox.Version, runtime.GOOS, runtime.GOARCH, root)

	if d, err := denobin.Find(); err != nil {
		c.fail("Deno (Host)", err.Error())
	} else if out, err := exec.Command(d, "--version").Output(); err != nil {
		c.fail("Deno (Host)", fmt.Sprintf("%s: %v", d, err))
	} else {
		c.ok("Deno (Host)", firstLine(out)+" - "+d)
	}

	state := tsbox.DefaultStateDir()
	if err := os.MkdirAll(state, 0o755); err != nil {
		c.fail("state", err.Error())
	} else if f, err := os.CreateTemp(state, ".doctor-*"); err != nil {
		c.fail("state", fmt.Sprintf("%s not writable: %v", state, err))
	} else {
		f.Close()
		os.Remove(f.Name())
		c.ok("state", state)
	}

	var isolated []string
	if runtime.GOOS == "linux" {
		isolated = doctorLinux(&c, root, state)
	} else {
		c.warn("isolation", "on "+runtime.GOOS+" there is only the dev driver (NO isolation)")
	}

	doctorDaemon(&c)

	fmt.Println()
	if len(isolated) > 0 {
		fmt.Printf("isolation available: yes (driver %s)\n", strings.Join(isolated, ", driver "))
	} else {
		fmt.Println("isolation available: no (driver dev) - only for your own, trusted code")
	}
	if c.fails > 0 {
		fmt.Printf("%d errors, %d warnings\n", c.fails, c.warns)
		return 1
	}
	return 0
}

func doctorLinux(c *checker, root bool, state string) []string {
	runsc, err := exec.LookPath("runsc")
	if err != nil {
		c.warn("runsc", "not in PATH (install gVisor: https://gvisor.dev/docs/user_guide/install/)")
	} else if out, err := exec.Command(runsc, "--version").Output(); err != nil {
		c.fail("runsc", fmt.Sprintf("%s: %v", runsc, err))
	} else {
		c.ok("runsc", firstLine(out)+" - "+runsc)
	}

	if b, err := os.ReadFile("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		c.warn("cgroup v2", "not found (/sys/fs/cgroup is not cgroup2)")
	} else {
		have := strings.Fields(string(b))
		var missing []string
		for _, want := range []string{"memory", "cpu", "pids"} {
			found := false
			for _, h := range have {
				found = found || h == want
			}
			if !found {
				missing = append(missing, want)
			}
		}
		if len(missing) > 0 {
			c.warn("cgroup v2", "controllers missing: "+strings.Join(missing, ", "))
		} else {
			c.ok("cgroup v2", "memory, cpu, pids")
		}
	}

	rfs := rootfsDir()
	if _, err := os.Stat(filepath.Join(rfs, profile.Deno)); err != nil {
		c.warn("rootfs", rfs+": no /usr/local/bin/deno (tsbox setup or scripts/build-rootfs.sh)")
	}
	for _, p := range profileSet().List() {
		st, _ := os.Stat(filepath.Join(p.Path, profile.Deno))
		c.ok("profile "+p.Name, fmt.Sprintf("%s (Debian %s, Deno %s, %d entries, hash %s)", p.Path, p.Debian, human(st.Size()), p.Entries, p.Hash))
	}

	if _, err := user.LookupGroup("tsbox"); err != nil {
		c.warn("group tsbox", "missing (only needed for the system daemon; tsbox setup creates it)")
	} else {
		c.ok("group tsbox", "present")
	}
	if _, err := os.Stat("/etc/systemd/system/tsboxd.service"); err == nil {
		c.ok("systemd", "tsboxd.service installed")
	} else {
		c.warn("systemd", "tsboxd.service not installed (optional; tsbox setup)")
	}

	if runtime.GOOS == "linux" {
		if f, err := os.OpenFile(gvisor.KVMDevice, os.O_RDWR, 0); err == nil {
			f.Close()
			c.ok("KVM", gvisor.KVMDevice+" usable (TSBOX_PLATFORM=kvm or tsbox daemon --platform kvm)")
		} else {
			c.ok("KVM", "not available, gVisor uses systrap ("+err.Error()+")")
		}
	}

	fcUsable := doctorFirecracker(c, root, rfs, state)
	if !root {
		c.warn("isolation", "gVisor and Firecracker need root; without root tsbox uses the dev driver")
		return nil
	}
	var usable []string
	if _, err := gvisor.New(gvisor.Config{RootFS: rfs, StateDir: filepath.Join(state, "gvisor"), Platform: os.Getenv("TSBOX_PLATFORM")}); err != nil {
		c.warn("isolation", err.Error())
	} else {
		c.ok("isolation", "gVisor usable")
		usable = append(usable, "gvisor")
	}
	if fcUsable {
		usable = append(usable, "firecracker")
	}
	return usable
}

// doctorFirecracker checks the optional firecracker driver (binary, guest kernel, e2fsprogs, KVM).
func doctorFirecracker(c *checker, root bool, rfs, state string) bool {
	bin := envOr("TSBOX_FIRECRACKER", "firecracker")
	kernel := envOr("TSBOX_FC_KERNEL", tsbox.DefaultFirecrackerKernel)
	p, err := exec.LookPath(bin)
	if err != nil {
		c.ok("firecracker", "not installed (optional driver; tsbox setup --firecracker)")
		return false
	}
	if out, err := exec.Command(p, "--version").Output(); err != nil {
		c.warn("firecracker", fmt.Sprintf("%s: %v", p, err))
	} else {
		c.ok("firecracker", firstLine(out)+" - "+p)
	}
	if _, err := os.Stat(kernel); err != nil {
		c.warn("fc kernel", kernel+" missing (TSBOX_FC_KERNEL or tsbox setup --firecracker)")
		return false
	}
	c.ok("fc kernel", kernel)
	for _, tool := range []string{"mkfs.ext4", "debugfs"} {
		if _, err := exec.LookPath(tool); err != nil {
			c.warn("e2fsprogs", tool+" missing (the firecracker driver builds the rootfs image with it)")
			return false
		}
	}
	jailer := os.Getenv("TSBOX_FC_JAILER")
	if jailer == "1" {
		jailer = "jailer"
	}
	if jailer != "" {
		if jp, err := exec.LookPath(jailer); err != nil {
			c.warn("fc jailer", "TSBOX_FC_JAILER is set, but "+jailer+" was not found")
		} else {
			c.ok("fc jailer", "on - "+jp+" (chroot, own UID per VM)")
		}
	} else if jp, err := exec.LookPath("jailer"); err == nil {
		c.ok("fc jailer", "available, off ("+jp+"; TSBOX_FC_JAILER=1 or tsbox daemon --fc-jailer 1)")
	} else {
		c.ok("fc jailer", "not installed (optional; tsbox setup --firecracker)")
	}
	if !root {
		return false
	}
	if _, err := firecracker.New(firecracker.Config{Firecracker: p, Kernel: kernel, RootFS: rfs, StateDir: filepath.Join(state, "firecracker"), Jailer: jailer}); err != nil {
		c.warn("isolation", err.Error())
		return false
	}
	c.ok("isolation", "Firecracker usable (TSBOX_DRIVER=firecracker or tsbox daemon --driver firecracker)")
	return true
}

func doctorDaemon(c *checker) {
	for _, h := range client.Hosts() {
		cl, err := client.New(h)
		if err != nil {
			c.fail("tsboxd", err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		info, err := cl.Info(ctx)
		cancel()
		cl.Close()
		sock, _ := client.SocketPath(h)
		if err != nil {
			if _, serr := os.Stat(sock); serr == nil {
				c.warn("tsboxd", fmt.Sprintf("%s present, but not reachable: %v", sock, err))
			}
			continue
		}
		perm, perr := daemon.SocketPerm(sock)
		if perr != nil {
			perm = perr.Error()
		}
		c.ok("tsboxd", fmt.Sprintf("%s, PID %d, %s daemon, driver %s, %d/%d boxes running", info.Version, info.PID, info.Mode, info.Driver, info.Running, info.Boxes))
		c.ok("socket", sock+" "+perm)
		return
	}
	if autostartAllowed() {
		c.ok("tsboxd", "not running; the CLI starts it on demand (TSBOX_NO_AUTOSTART=1 turns this off)")
	} else {
		c.warn("tsboxd", "not running and autostart is off")
	}
}

const systemdUnit = `[Unit]
Description=tsbox Daemon (tsboxd)
Documentation=https://github.com/rif223/tsbox
After=local-fs.target

[Service]
ExecStart=%s daemon --system
Restart=on-failure
KillMode=mixed
# When the host runs out of memory, the kernel hits boxes first (oom_score_adj 500), not tsboxd.
OOMScoreAdjust=-500
TimeoutStopSec=30

[Install]
WantedBy=multi-user.target
`

// setup sets up rootfs, group and systemd unit on Linux.
func setupCmd(args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "only show what would happen")
	noRootfs := fs.Bool("no-rootfs", false, "do not build the rootfs")
	noSystemd := fs.Bool("no-systemd", false, "no systemd unit")
	withFC := fs.Bool("firecracker", false, "also install the firecracker binary and the guest kernel (optional driver)")
	usageFor(fs, "setup [--dry-run] [--no-rootfs] [--no-systemd] [--firecracker]")
	if _, ok := parsePos(fs, args, 0, 0); !ok {
		return 2
	}
	if runtime.GOOS != "linux" {
		fmt.Printf("Nothing to set up on %s: tsbox uses the dev driver (NO isolation) and starts tsboxd itself on demand.\n\n", runtime.GOOS)
		return doctorCmd(nil)
	}
	if os.Geteuid() != 0 && !*dry {
		fmt.Fprintln(os.Stderr, "tsbox setup: please run as root (sudo)")
		return 1
	}
	step := func(desc string, cmd ...string) error {
		fmt.Println("==", desc)
		if len(cmd) == 0 {
			return nil
		}
		fmt.Println("  $", strings.Join(cmd, " "))
		if *dry {
			return nil
		}
		x := exec.Command(cmd[0], cmd[1:]...)
		x.Stdout, x.Stderr = os.Stdout, os.Stderr
		return x.Run()
	}
	fail := func(err error) int {
		fmt.Fprintln(os.Stderr, "tsbox setup:", err)
		return 1
	}
	rfs := rootfsDir()
	if !*noRootfs {
		if _, err := os.Stat(filepath.Join(rfs, "usr/local/bin/deno")); err == nil {
			fmt.Println("== rootfs present:", rfs)
		} else {
			deno, err := denobin.Find()
			if err != nil {
				return fail(err)
			}
			script := filepath.Join(os.TempDir(), "tsbox-build-rootfs.sh")
			if !*dry {
				if err := os.WriteFile(script, []byte(scripts.BuildRootfs), 0o700); err != nil {
					return fail(err)
				}
				defer os.Remove(script)
			}
			os.Setenv("OUT", rfs)
			os.Setenv("DENO", deno)
			if err := step("build the rootfs (debootstrap + Deno) at "+rfs, "bash", script); err != nil {
				return fail(err)
			}
		}
	}
	if *withFC {
		if err := setupFirecracker(*dry); err != nil {
			return fail(err)
		}
	}
	if _, err := user.LookupGroup("tsbox"); err != nil {
		if err := step("create the group tsbox (access to tsboxd, corresponds to the docker group)", "groupadd", "--system", "tsbox"); err != nil {
			return fail(err)
		}
	}
	if !*noSystemd {
		if _, err := os.Stat("/run/systemd/system"); err != nil {
			fmt.Println("== systemd is not running: unit skipped. Start the daemon manually: tsbox daemon --system")
		} else {
			exe, err := os.Executable()
			if err != nil {
				return fail(err)
			}
			unit := fmt.Sprintf(systemdUnit, exe)
			_ = step("write the systemd unit: /etc/systemd/system/tsboxd.service (ExecStart=" + exe + " daemon --system)")
			if !*dry {
				if err := os.WriteFile("/etc/systemd/system/tsboxd.service", []byte(unit), 0o644); err != nil {
					return fail(err)
				}
			}
			if err := step("reload systemd", "systemctl", "daemon-reload"); err != nil {
				return fail(err)
			}
			if err := step("enable and start tsboxd", "systemctl", "enable", "--now", "tsboxd"); err != nil {
				return fail(err)
			}
		}
	}
	fmt.Println("\nGrant users access (as with docker): usermod -aG tsbox <user>")
	if *dry {
		return 0
	}
	fmt.Println()
	return doctorCmd(nil)
}
