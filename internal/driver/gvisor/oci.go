package gvisor

import (
	"strconv"

	"github.com/rif223/tsbox/internal/driver"
)

const cpuPeriod = 100_000 // µs

// ociSpec creates the OCI profile of a box:
//   - init runs as UID 0 of the sandbox with only SETUID/SETGID/KILL (UID per script, spike b), no_new_privs
//   - rootfs read-only; no writable mounts: no /tmp (spike, finding 4), /dev as a 1 MiB tmpfs
//   - /artifacts: only this box's artifacts, ro; /.tsbox: the tsbox binary (init), ro
//   - no network (runsc --network=none), namespaces pid/ipc/uts/mount/network
//   - rootfs from the box's profile (Spec.RootFS), otherwise Config.RootFS; oom_score_adj for sentry and gofer
//   - cgroup v2: memory.max without swap, cpu.max, pids.max (runsc creates the cgroup; never create it
//     beforehand, otherwise runsc sets no limits, spike f)
func ociSpec(cfg Config, s driver.Spec, initDir string) map[string]any {
	rootfs := cfg.RootFS
	if s.RootFS != "" {
		rootfs = s.RootFS
	}
	caps := []string{"CAP_SETUID", "CAP_SETGID", "CAP_KILL"}
	mounts := []map[string]any{
		{"destination": "/proc", "type": "proc", "source": "proc"},
		{"destination": "/dev", "type": "tmpfs", "source": "tmpfs", "options": []string{"nosuid", "noexec", "mode=755", "size=1m"}},
		// Without its own entry, runsc mounts a writable tmpfs at /tmp (shared by all scripts): explicitly
		// empty and read-only.
		{"destination": "/tmp", "type": "tmpfs", "source": "tmpfs", "options": []string{"ro", "nosuid", "nodev", "noexec", "size=4k", "mode=555"}},
		{"destination": dirArtifacts, "type": "bind", "source": s.ArtifactsDir, "options": []string{"rbind", "ro", "nosuid", "nodev", "noexec"}},
		{"destination": dirInit, "type": "bind", "source": initDir, "options": []string{"rbind", "ro", "nosuid", "nodev"}},
	}
	return map[string]any{
		"ociVersion": "1.0.2",
		"process": map[string]any{
			"terminal": false,
			"user":     map[string]any{"uid": 0, "gid": 0},
			"args":     initArgs(s),
			"env":      []string{"NO_COLOR=1"},
			"cwd":      "/",
			"capabilities": map[string]any{
				"bounding": caps, "effective": caps, "permitted": caps, "inheritable": []string{}, "ambient": []string{},
			},
			"noNewPrivileges": true,
			// runsc applies the value to sentry and gofer (host OOM hits boxes first, not tsboxd).
			"oomScoreAdj": s.OOMScoreAdj,
			"rlimits": []map[string]any{
				{"type": "RLIMIT_NOFILE", "hard": 1024, "soft": 1024},
				{"type": "RLIMIT_CORE", "hard": 0, "soft": 0},
			},
		},
		"root":     map[string]any{"path": rootfs, "readonly": true},
		"hostname": s.ID,
		"mounts":   mounts,
		"linux": map[string]any{
			"namespaces": []map[string]string{
				{"type": "pid"}, {"type": "ipc"}, {"type": "uts"}, {"type": "mount"}, {"type": "network"},
			},
			"cgroupsPath": cfg.CgroupParent + "/" + s.ID,
			"resources": map[string]any{
				"memory": map[string]any{"limit": s.Limits.MemoryBytes, "swap": s.Limits.MemoryBytes},
				"cpu":    map[string]any{"quota": int64(s.Limits.CPUCores * cpuPeriod), "period": cpuPeriod},
				"pids":   map[string]any{"limit": s.Limits.PIDs},
			},
		},
	}
}

// initArgs is the command line of init in the sandbox.
func initArgs(s driver.Spec) []string {
	args := []string{dirInit + "/tsbox", "__init", "--deno", sandboxDeno, "--uid-mode"}
	if s.BaselineRSS > 0 {
		args = append(args, "--baseline-rss", strconv.FormatInt(s.BaselineRSS, 10))
	}
	return args
}
