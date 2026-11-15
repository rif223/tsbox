package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/rif223/tsbox"
	"github.com/rif223/tsbox/client"
	"github.com/rif223/tsbox/internal/daemon"
)

// defaultIdle is the idle time after which an autostarted daemon exits.
const defaultIdle = 5 * time.Minute

// daemonCmd: tsbox daemon [--system] [--socket] [--idle] (tsboxd), tsbox daemon stop [-f].
func daemonCmd(args []string) int {
	if len(args) > 0 && args[0] == "stop" {
		return daemonStop(args[1:])
	}
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	system := fs.Bool("system", false, "system daemon (Linux, root): socket "+client.SystemSocket+" for the group --group")
	socket := fs.String("socket", "", "path of the socket (default: system or user socket)")
	group := fs.String("group", "tsbox", "group with access to the system socket (corresponds to the docker group)")
	idle := fs.Duration("idle", 0, "exit after this much idle time (0 = never)")
	auto := fs.Bool("autostarted", false, "started by the CLI (internal)")
	drv := fs.String("driver", envOr("TSBOX_DRIVER", "auto"), "driver: auto | gvisor | firecracker | dev (TSBOX_DRIVER sets the default)")
	fcJailer := fs.String("fc-jailer", "", "firecracker: run every VMM under the jailer (path, or 1 = jailer from PATH; default: TSBOX_FC_JAILER)")
	fcKernel := fs.String("fc-kernel", "", "guest kernel of the firecracker driver (default: TSBOX_FC_KERNEL or "+tsbox.DefaultFirecrackerKernel+")")
	rootfs := fs.String("rootfs", "", "rootfs for gVisor (profile default; default: TSBOX_ROOTFS)")
	profiles := fs.String("profiles", "", "directory of named profiles (default: TSBOX_PROFILES or /var/lib/tsbox/profiles)")
	platform := fs.String("platform", "", "gVisor platform: systrap | kvm (default: TSBOX_PLATFORM or systrap)")
	oomAdj := fs.Int("oom-score-adj", tsbox.DefaultOOMScoreAdj, "oom_score_adj of the box processes (Linux; 0 = unchanged)")
	usageFor(fs, "daemon [flags] | daemon stop [-f]   (tsboxd: local API for ps, logs, stats, ...)")
	if _, ok := parsePos(fs, args, 0, 0); !ok {
		return 2
	}
	switch *drv {
	case "auto", "gvisor", "firecracker", "dev":
	default:
		fmt.Fprintf(os.Stderr, "tsbox daemon: --driver %q: allowed are auto, gvisor, firecracker and dev\n", *drv)
		return 2
	}
	if runtime.GOOS == "windows" && *socket != "" && !strings.HasPrefix(*socket, `\\.\pipe\`) {
		fmt.Fprintf(os.Stderr, "tsbox daemon: --socket %s: on Windows a named pipe is expected, e.g. \\\\.\\pipe\\tsboxd-test\n", *socket)
		return 2
	}
	switch *platform {
	case "", "systrap", "kvm":
	default:
		fmt.Fprintf(os.Stderr, "tsbox daemon: --platform %q: allowed are systrap and kvm\n", *platform)
		return 2
	}
	if *system && runtime.GOOS != "linux" {
		fmt.Fprintln(os.Stderr, "tsbox daemon: --system exists only on Linux")
		return 2
	}
	if *system && os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "tsbox daemon: --system needs root")
		return 2
	}
	cfg := daemon.Config{StateDir: tsbox.DefaultStateDir(), Socket: *socket, System: *system, Group: *group,
		Idle: *idle, Autostarted: *auto,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))}
	if cfg.Socket == "" {
		if *system {
			cfg.Socket = client.SystemSocket
		} else {
			cfg.Socket = client.UserSocket(cfg.StateDir)
		}
	}
	cfg.Engine = append(cfg.Engine, tsbox.WithDriver(*drv))
	if *rootfs != "" {
		cfg.Engine = append(cfg.Engine, tsbox.WithRootFS(*rootfs))
	}
	if *profiles != "" {
		cfg.Engine = append(cfg.Engine, tsbox.WithProfilesDir(*profiles))
	}
	cfg.Engine = append(cfg.Engine, tsbox.WithOOMScoreAdj(*oomAdj))
	if *platform != "" {
		cfg.Engine = append(cfg.Engine, tsbox.WithGVisorPlatform(*platform))
	}
	if *fcKernel != "" {
		cfg.Engine = append(cfg.Engine, tsbox.WithFirecrackerKernel(*fcKernel))
	}
	if *fcJailer != "" {
		if *fcJailer == "1" {
			*fcJailer = "jailer"
		}
		cfg.Engine = append(cfg.Engine, tsbox.WithFirecrackerJailer(*fcJailer))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := daemon.Run(ctx, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "tsboxd:", err)
		return 1
	}
	return 0
}

// connect connects to the daemon and starts a user daemon if needed (except with TSBOX_NO_AUTOSTART=1 or
// TSBOX_HOST).
func connect(ctx context.Context) (*client.Client, client.Info, error) {
	hosts := client.Hosts()
	var hint string
	for _, h := range hosts {
		c, err := client.New(h)
		if err != nil {
			return nil, client.Info{}, err
		}
		info, err := ping(ctx, c)
		if err == nil {
			if info.Autostarted && info.Build != daemon.BuildID() && info.Running == 0 && autostartAllowed() {
				// Older or newer binary without running boxes: replace it.
				_ = c.Shutdown(ctx)
				c.Close()
				waitGone(ctx, c)
				break
			}
			if info.API != client.APIVersion {
				c.Close()
				return nil, info, fmt.Errorf("tsboxd speaks API v%d, this CLI v%d (restart tsboxd)", info.API, client.APIVersion)
			}
			return c, info, nil
		}
		c.Close()
		if errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EACCES) {
			hint = fmt.Sprintf(" (%s: permission denied - member of the group tsbox?)", h)
		}
	}
	if !autostartAllowed() {
		start := `"tsbox daemon"`
		if runtime.GOOS == "linux" {
			start += ` or "systemctl start tsboxd"`
		}
		return nil, client.Info{}, fmt.Errorf("tsboxd not reachable%s. Start it with %s (autostart is off)", hint, start)
	}
	host := hosts[len(hosts)-1]
	sock, _ := client.SocketPath(host)
	logPath := filepath.Join(tsbox.DefaultStateDir(), "tsboxd.log")
	var logStart int64
	if st, err := os.Stat(logPath); err == nil {
		logStart = st.Size()
	}
	exited, err := spawnDaemon(sock)
	if err != nil {
		return nil, client.Info{}, fmt.Errorf("starting tsboxd: %w", err)
	}
	c, _ := client.New(host)
	deadline := time.Now().Add(60 * time.Second) // an ending predecessor holds the state for up to 40 s
	for {
		info, err := ping(ctx, c)
		if err == nil {
			return c, info, nil
		}
		select {
		case <-exited: // the daemon ended without answering: show why
			if msg := lastLogLine(logPath, logStart); msg != "" {
				return nil, client.Info{}, errors.New(msg)
			}
			return nil, client.Info{}, fmt.Errorf("tsboxd ended right after the start; see %s", logPath)
		default:
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, client.Info{}, fmt.Errorf("tsboxd does not answer (%v); see %s", err, logPath)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// lastLogLine returns the last non-empty line written to the log after offset (the error of a daemon that
// could not start, e.g. "tsboxd: gvisor: runsc not found ...").
func lastLogLine(path string, offset int64) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if int64(len(b)) < offset { // truncated at the start (log above 10 MiB)
		offset = 0
	}
	lines := strings.Split(strings.TrimSpace(string(b[offset:])), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// daemonStop: tsbox daemon stop [-f] ends the user daemon (the system daemon belongs to systemd).
func daemonStop(args []string) int {
	fs := flag.NewFlagSet("daemon stop", flag.ContinueOnError)
	force := fs.Bool("f", false, "also with running boxes (they are stopped)")
	usageFor(fs, "daemon stop [-f]")
	if _, ok := parsePos(fs, args, 0, 0); !ok {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, h := range client.Hosts() {
		c, err := client.New(h)
		if err != nil {
			continue
		}
		info, err := ping(ctx, c)
		if err != nil {
			c.Close()
			continue
		}
		defer c.Close()
		if info.Mode == "system" {
			fmt.Fprintln(os.Stderr, "tsbox: the system daemon is managed by systemd: systemctl stop tsboxd")
			return 1
		}
		if info.Running > 0 && !*force {
			fmt.Fprintf(os.Stderr, "tsbox: %d box(es) running; stop them first or use \"tsbox daemon stop -f\"\n", info.Running)
			return 1
		}
		if err := c.Shutdown(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "tsbox:", err)
			return 1
		}
		waitGone(ctx, c)
		fmt.Printf("tsboxd stopped (PID %d)\n", info.PID)
		return 0
	}
	fmt.Println("tsboxd is not running")
	return 0
}

func autostartAllowed() bool {
	return os.Getenv("TSBOX_NO_AUTOSTART") != "1" && os.Getenv("TSBOX_HOST") == ""
}

func ping(ctx context.Context, c *client.Client) (client.Info, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return c.Info(ctx)
}

func waitGone(ctx context.Context, c *client.Client) {
	for i := 0; i < 100; i++ {
		if _, err := ping(ctx, c); err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// spawnDaemon starts "tsbox daemon --autostarted" detached; output to <state>/tsboxd.log. The channel is
// closed when the daemon process ends (only relevant while the CLI waits for it).
func spawnDaemon(sock string) (<-chan struct{}, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	state := tsbox.DefaultStateDir()
	if err := os.MkdirAll(state, 0o755); err != nil {
		return nil, err
	}
	logPath := filepath.Join(state, "tsboxd.log")
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if st, err := os.Stat(logPath); err == nil && st.Size() > 10<<20 {
		flags |= os.O_TRUNC
	}
	lf, err := os.OpenFile(logPath, flags, 0o600)
	if err != nil {
		return nil, err
	}
	defer lf.Close()
	idle := defaultIdle
	if v := os.Getenv("TSBOX_IDLE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			idle = d
		}
	}
	cmd, err := startDetached(func() *exec.Cmd {
		cmd := exec.Command(exe, "daemon", "--autostarted", "--idle", idle.String(), "--socket", sock)
		cmd.Stdout, cmd.Stderr = lf, lf
		cmd.Dir = state
		return cmd
	})
	if err != nil {
		return nil, err
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	return exited, nil
}

// releaseSDK asks every reachable tsboxd (without starting one) to end the provider processes of an installed
// SDK, so that sdk add/rm can replace or remove its files. A running box that uses the SDK stops the command.
func releaseSDK(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, h := range client.Hosts() {
		c, err := client.New(h)
		if err != nil {
			continue
		}
		if _, err := ping(ctx, c); err == nil {
			var ae *client.Error
			if err := c.ReleaseSDK(ctx, name); errors.As(err, &ae) && ae.Code == "in_use" {
				c.Close()
				return fmt.Errorf("%s (stop them first)", ae.Message)
			}
		}
		c.Close()
	}
	return nil
}
