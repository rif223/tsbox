// Package client talks to the local API of tsboxd (HTTP/JSON over a Unix socket; on Windows over a named
// pipe with a DACL for the current user only).
//
//	c, _ := client.New(client.Hosts()[0])
//	boxes, _ := c.List(ctx, true)
//
// Whoever can access the socket may start boxes with the daemon's rights (like the "docker" group).
package client

import (
	"encoding/json"
	"time"

	"github.com/rif223/tsbox"
)

// APIVersion is in the path (/v1/…) and in Info.API.
const APIVersion = 1

// Info describes the daemon.
type Info struct {
	Version     string `json:"version"`
	API         int    `json:"api"`
	Build       string `json:"build"` // identifier of the binary (size and modification time)
	PID         int    `json:"pid"`
	Mode        string `json:"mode"` // "user" or "system"
	Autostarted bool   `json:"autostarted"`
	Idle        string `json:"idle,omitempty"` // idle time until the daemon ends ("" = never)
	// Drivers lists all drivers of this platform and whether boxes can use them. Every box names its own;
	// there is no default driver.
	Drivers  []tsbox.DriverInfo `json:"drivers,omitempty"`
	StateDir string             `json:"stateDir"`
	Socket   string             `json:"socket"`
	Running  int                `json:"running"` // boxes that have not ended
	Boxes    int                `json:"boxes"`
}

// CreateRequest creates a box (POST /v1/boxes).
type CreateRequest struct {
	Spec tsbox.BoxSpec `json:"spec"`
	// Start starts the box after the build.
	Start bool `json:"start"`
	// AutoRemove removes the box as soon as it ends (like docker run --rm).
	AutoRemove bool `json:"autoRemove,omitempty"`
	// Attach turns the response into a stream of AttachMsg (logs, events, final status). If the connection
	// breaks before the end, the daemon stops the box (and removes it with AutoRemove).
	Attach bool `json:"attach,omitempty"`
	// Env holds the values of the "env:<NAME>" secrets named in the specification, from the caller's
	// environment. The daemon keeps them in memory only; they never appear in the state, inspect or logs.
	Env map[string]string `json:"env,omitempty"`
}

// UpdateRequest replaces the specification of a box (PUT /v1/boxes/{name}). The daemon builds and checks
// the new specification while the old box keeps running, then stops it and starts the new one if the old
// one was running. Name, storage area and log files stay.
type UpdateRequest struct {
	Spec tsbox.BoxSpec `json:"spec"`
	// Env adds or replaces values of env: secrets (see CreateRequest.Env); known values are kept.
	Env map[string]string `json:"env,omitempty"`
	// Grace is the deadline for the old scripts, e.g. "10s" ("" = default).
	Grace string `json:"grace,omitempty"`
}

// EmitRequest sends an event to the scripts of a running box (POST /v1/boxes/{name}/emit). Defaults: SDK
// "inbox", event "message"; payload for inbox is {"topic": "...", "data": ...}.
type EmitRequest struct {
	SDK     string          `json:"sdk,omitempty"`
	Event   string          `json:"event,omitempty"`
	Script  string          `json:"script,omitempty"` // "" = all scripts that subscribed
	Payload json.RawMessage `json:"payload,omitempty"`
}

// EmitResult says to how many scripts the event was queued.
type EmitResult struct {
	Delivered int `json:"delivered"`
}

// Box is a box from the daemon's point of view (ps).
type Box struct {
	tsbox.BoxStatus
	Created    time.Time `json:"created"`
	AutoRemove bool      `json:"autoRemove,omitempty"`
	Desired    string    `json:"desired"` // "running" or "stopped"
	Loaded     bool      `json:"loaded"`  // built in the engine (otherwise only the stored specification)
}

// Inspect is the full view of a box, without secret values.
type Inspect struct {
	Box
	Spec   tsbox.BoxSpec `json:"spec"`
	Dir    string        `json:"dir"`
	LogDir string        `json:"logDir"`
	// EnvSecrets names the env: secrets whose values the daemon keeps in memory.
	EnvSecrets []string `json:"envSecrets,omitempty"`
}

// StatsEntry are measurements of a running box.
type StatsEntry struct {
	Box       string               `json:"box"`
	State     tsbox.BoxState       `json:"state"`
	Started   time.Time            `json:"started"`
	Limits    tsbox.Limits         `json:"limits"`
	RSSLimits map[string]int64     `json:"rssLimits,omitempty"` // per script, if set
	Scripts   []tsbox.ScriptStatus `json:"scripts"`
	Stats     tsbox.BoxStats       `json:"stats"`
	Error     string               `json:"error,omitempty"`
}

// AttachMsg is an element of the attach stream.
type AttachMsg struct {
	Type   string           `json:"type"` // created, log, event, exit, error
	Box    *Box             `json:"box,omitempty"`
	Log    *tsbox.LogLine   `json:"log,omitempty"`
	Event  *tsbox.Event     `json:"event,omitempty"`
	Status *tsbox.BoxStatus `json:"status,omitempty"`
	Error  *Error           `json:"error,omitempty"`
}

// Error is an API error.
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"` // not_found, conflict, invalid, build, internal
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// IsNotFound reports whether err means "box not found".
func IsNotFound(err error) bool {
	ae, ok := err.(*Error)
	return ok && ae.Code == "not_found"
}

// More event kinds of the daemon (in addition to the engine's).
const (
	EventBoxCreated tsbox.EventKind = "box_created"
	EventBoxRemoved tsbox.EventKind = "box_removed"
	EventBoxStopped tsbox.EventKind = "box_stopped"
	EventBoxUpdated tsbox.EventKind = "box_updated"
)

// Label for projects (tsbox.json).
const (
	LabelProject  = "tsbox.project"
	LabelService  = "tsbox.box"
	LabelSpecHash = "tsbox.spec-hash"
	LabelDir      = "tsbox.project-dir" // directory of the project file (two projects with the same name)
)

// KVPage is a page of the entries of one kv namespace (GET /v1/kv/{storage}/{script}).
type KVPage struct {
	Entries []tsbox.KVEntry `json:"entries"`
	More    bool            `json:"more"` // further entries follow: ask again with after = the last key
}
