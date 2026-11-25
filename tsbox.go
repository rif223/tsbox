// Package tsbox runs TypeScript code (Deno) in isolation: one sandbox with hard limits per box, inside it one
// Deno process per script with its own identity and its own limits. Drivers: gvisor (Linux, isolated) and
// dev (all systems, not isolated).
//
//	e, _ := tsbox.New(ctx)
//	b, _ := e.Run(ctx, tsbox.BoxSpec{Name: "hello", Scripts: []tsbox.ScriptSpec{{Name: "main", Entry: "hello.ts"}}})
//	res, _ := b.Wait(ctx)
//
// Applications that embed tsbox call tsbox.Init() at the beginning of main: their own binary serves as
// tsbox-init in the box.
package tsbox

import (
	"encoding/json"
	"os"
	"time"

	"github.com/rif223/tsbox/internal/fcguest"
	"github.com/rif223/tsbox/internal/initbox"
)

// Version of tsbox. Release builds set it from the tag:
//
//	go build -ldflags "-X github.com/rif223/tsbox.Version=0.3.0" ./cmd/tsbox
var Version = "0.3.0-dev"

// Init runs the internal roles of the binary (tsbox-init in the box, UID switch, test probe) and then
// exits the process. Otherwise it returns immediately. Must be at the beginning of main.
func Init() {
	if len(os.Args) < 2 {
		return
	}
	switch os.Args[1] {
	case "__init":
		os.Exit(initbox.Main(os.Args[2:]))
	case "__exec":
		os.Exit(initbox.ExecMain(os.Args[2:]))
	case "__oomexec":
		os.Exit(initbox.OOMExecMain(os.Args[2:]))
	case "__probe":
		os.Exit(initbox.ProbeMain(os.Args[2:]))
	case "__fcinit":
		os.Exit(fcguest.InitMain(os.Args[2:]))
	case "__fcexec":
		os.Exit(fcguest.ExecMain(os.Args[2:]))
	}
}

// Limits are hard limits of the whole box.
type Limits struct {
	Memory int64   `json:"memory"` // bytes, without swap
	CPU    float64 `json:"cpu"`    // cores, e.g. 0.5
	PIDs   int     `json:"pids"`   // processes + threads (on Windows: processes)
	// StartCPU (opt-in, 0 = off) is a temporary CPU boost: the box starts with StartCPU cores and falls back
	// to CPU as soon as all scripts are ready or have ended, after 30 s at the latest. Restarts of single
	// scripts get no boost. If the limit cannot be reset, the box is ended.
	StartCPU float64 `json:"startCpu,omitempty"`
}

// ScriptLimits are limits per script. 0 means the default (heap, watchdog) or no limit of its own.
type ScriptLimits struct {
	Heap     int64         `json:"heap"`     // V8 heap (default 64 MiB)
	RSS      int64         `json:"rss"`      // private memory; 0 = only the box limit
	Threads  int           `json:"threads"`  // default 32; not enforced on Windows
	Timeout  time.Duration `json:"timeout"`  // run time; 0 = unlimited
	Watchdog time.Duration `json:"watchdog"` // blocked event loop; default 10 s, <0 = off
}

// RestartMode is the restart policy of a script.
type RestartMode string

const (
	RestartNo        RestartMode = "no"
	RestartOnFailure RestartMode = "on-failure"
	RestartAlways    RestartMode = "always"
)

// RestartPolicy with backoff (BackoffMin · 2^(n−1) up to BackoffMax) and circuit breaker: MaxFailures
// failures within Window ⇒ script "broken" until the next start of the box.
type RestartPolicy struct {
	Mode        RestartMode   `json:"mode"`
	MaxFailures int           `json:"maxFailures"` // default 5
	Window      time.Duration `json:"window"`      // default 10 min
	BackoffMin  time.Duration `json:"backoffMin"`  // default 1 s
	BackoffMax  time.Duration `json:"backoffMax"`  // default 60 s
}

// ScriptSpec describes a script of a box.
type ScriptSpec struct {
	Name    string          `json:"name"`  // [a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}
	Entry   string          `json:"entry"` // path to the .ts file
	Params  json.RawMessage `json:"params,omitempty"`
	Limits  ScriptLimits    `json:"limits"`
	Restart *RestartPolicy  `json:"restart,omitempty"` // nil = the box's policy

	debugExec []string // tests only: replaces the Deno invocation (escape simulation)
}

// BoxSpec describes a box.
type BoxSpec struct {
	Name    string            `json:"name"` // [a-z0-9-]{1,48}
	Labels  map[string]string `json:"labels,omitempty"`
	Limits  Limits            `json:"limits"`
	Restart RestartPolicy     `json:"restart"`
	Scripts []ScriptSpec      `json:"scripts"`
	SDKs    []SDKUse          `json:"sdks,omitempty"`
	// Storage is the storage area of stateful SDKs (kv). Default: the box name. Boxes with the same
	// Storage see the same data (separated per script name).
	Storage string `json:"storage,omitempty"`
	// Profile selects the box's rootfs under gVisor: "" or "default", or a named profile
	// (<profiles dir>/<name>). The dev driver uses the host's Deno and ignores it.
	Profile string `json:"profile,omitempty"`
	// Driver runs the box: "gvisor", "firecracker" or "dev". Required: there is no default driver.
	Driver string `json:"driver,omitempty"`
}

// ExitReason explains why a script ended.
type ExitReason string

const (
	ReasonExit         ExitReason = "exit"     // code 0
	ReasonCrash        ExitReason = "crash"    // code ≠ 0
	ReasonHeap         ExitReason = "heap"     // V8 heap full
	ReasonRSS          ExitReason = "rss"      // memory limit of the script
	ReasonThreads      ExitReason = "threads"  // thread limit
	ReasonOOM          ExitReason = "oom"      // box memory full, the kernel chose this script
	ReasonWatchdog     ExitReason = "watchdog" // event loop blocked
	ReasonTimeout      ExitReason = "timeout"  // run time limit
	ReasonIntegrity    ExitReason = "integrity"
	ReasonProtocol     ExitReason = "protocol"
	ReasonBackpressure ExitReason = "backpressure"
	ReasonSpawnFailed  ExitReason = "spawn_failed"
	ReasonKilled       ExitReason = "killed" // signal without a known reason
	ReasonStopped      ExitReason = "stopped"
	ReasonBoxHung      ExitReason = "box_hung" // box watchdog
	ReasonBoxDied      ExitReason = "box_died"
	ReasonBoxShutdown  ExitReason = "box_shutdown"
)

// Failure reports whether the reason counts as a failure (restart with on-failure, breaker).
func (r ExitReason) Failure() bool {
	switch r {
	case ReasonExit, ReasonStopped, ReasonBoxShutdown:
		return false
	}
	return true
}

// ExitStatus is the end of a script run.
type ExitStatus struct {
	At     time.Time  `json:"at"`
	Reason ExitReason `json:"reason"`
	Code   int        `json:"code"`
	Signal string     `json:"signal,omitempty"`
	Detail string     `json:"detail,omitempty"`
}

// ScriptState is the state of a script.
type ScriptState string

const (
	ScriptPending  ScriptState = "pending"
	ScriptStarting ScriptState = "starting"
	ScriptRunning  ScriptState = "running"
	ScriptReady    ScriptState = "ready"
	ScriptBackoff  ScriptState = "backoff"
	ScriptExited   ScriptState = "exited"
	ScriptBroken   ScriptState = "broken" // breaker open, integrity or protocol error
)

// BoxState is the state of a box.
type BoxState string

const (
	BoxCreated     BoxState = "created"
	BoxStarting    BoxState = "starting"
	BoxRunning     BoxState = "running"
	BoxStopping    BoxState = "stopping"
	BoxExited      BoxState = "exited"
	BoxFailed      BoxState = "failed"
	BoxQuarantined BoxState = "quarantined" // hung or died; no automatic restart
)

// EventKind is the kind of an event.
type EventKind string

const (
	EventBoxStarted      EventKind = "box_started"
	EventBoxExited       EventKind = "box_exited"
	EventBoxQuarantined  EventKind = "box_quarantined"
	EventScriptStarted   EventKind = "script_started"
	EventScriptReady     EventKind = "script_ready"
	EventScriptExited    EventKind = "script_exited"
	EventRestartPlanned  EventKind = "restart_scheduled"
	EventCircuitOpen     EventKind = "circuit_open"
	EventIntegrityFailed EventKind = "integrity_failed"
	EventOOM             EventKind = "oom"
	EventViolation       EventKind = "violation"
)

// Event is an event for embedding applications (asynchronous, never blocks the engine).
type Event struct {
	At     time.Time   `json:"at"`
	Kind   EventKind   `json:"kind"`
	Box    string      `json:"box"`
	Script string      `json:"script,omitempty"`
	Exit   *ExitStatus `json:"exit,omitempty"`
	Detail string      `json:"detail,omitempty"`
}

// LogLine is a log line of a script. Level: debug, info, warn, error, stderr.
type LogLine struct {
	Seq    int64     `json:"seq"` // strictly monotonic per engine (also across restarts)
	At     time.Time `json:"at"`
	Box    string    `json:"box"`
	Script string    `json:"script"`
	Level  string    `json:"level"`
	Msg    string    `json:"msg"`
}

// ScriptStatus is the view of a script.
type ScriptStatus struct {
	Name     string      `json:"name"`
	State    ScriptState `json:"state"`
	Restarts int         `json:"restarts"`
	Started  time.Time   `json:"started,omitempty"`
	LastExit *ExitStatus `json:"lastExit,omitempty"`
	SHA256   string      `json:"sha256"`
}

// BoxStatus is the view of a box.
type BoxStatus struct {
	Name     string            `json:"name"`
	Driver   string            `json:"driver"`
	Isolated bool              `json:"isolated"`
	State    BoxState          `json:"state"`
	Error    string            `json:"error,omitempty"`
	Started  time.Time         `json:"started,omitempty"`
	Limits   Limits            `json:"limits"`
	Labels   map[string]string `json:"labels,omitempty"`
	// Profile and ProfileHash (fingerprint of the rootfs at the last start; gVisor only).
	Profile     string         `json:"profile"`
	ProfileHash string         `json:"profileHash,omitempty"`
	Scripts     []ScriptStatus `json:"scripts"`
	SDKs        []SDKInfo      `json:"sdks,omitempty"`
	// DroppedEvents counts SDK events that were discarded because of a full queue.
	DroppedEvents uint64 `json:"droppedEvents,omitempty"`
}

// ScriptStats are measurements of a script (RSS/threads measured by init, heap reported by the script).
type ScriptStats struct {
	Name     string  `json:"name"`
	RSS      int64   `json:"rss"`
	CPUCores float64 `json:"cpuCores"`
	Threads  int     `json:"threads"`
	HeapUsed int64   `json:"heapUsed"`
}

// BoxStats are measurements of a box.
type BoxStats struct {
	At         time.Time     `json:"at"`
	Memory     int64         `json:"memory"`
	MemoryPeak int64         `json:"memoryPeak"`
	CPUCores   float64       `json:"cpuCores"`
	PIDs       int           `json:"pids"`
	OOMKills   int           `json:"oomKills"`
	Scripts    []ScriptStats `json:"scripts"`
}
