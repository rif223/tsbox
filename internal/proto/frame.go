// Package proto is tsbox's frame protocol (protocol v2): host ⇄ tsbox-init over the box's stdin/stdout and
// tsbox-init ⇄ script over the pipes of each script process. A frame is "u32 length (big endian) | JSON
// object", at most 1 MiB. js/core/frames.ts implements the same encoding.
//
// Frames from the box are untrusted: Read limits the size, rejects unknown fields and types and never
// panics on any input (FuzzRead).
package proto

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Version is the protocol version init reports in hello.
const Version = 2

// MaxFrame is the largest allowed frame length (without the length prefix).
const MaxFrame = 1 << 20

// MaxID is the largest correlation ID: JavaScript numbers are exact only up to 2^53−1.
const MaxID = 1<<53 - 1

// Frame types.
const (
	// host → init
	TStart = "start" // start a script (SID, P: Start)
	TStop  = "stop"  // stop a script (SID)
	// host → init → script
	TRet   = "ret"   // answer to call (SID, ID, P | Err)
	TEvent = "event" // SDK event (SDK, Name, P; SIDs: recipients, host → init only)
	// host ⇄ init (box watchdog)
	TPing = "ping"
	TPong = "pong"

	// init → host
	THello  = "hello"  // init is ready (P: Hello)
	TExit   = "exit"   // script ended (SID, P: Exit)
	TPStats = "pstats" // measurements of all scripts (P: []ProcStats)
	// script → init → host
	TReady = "ready"
	TLog   = "log"   // Level, Msg
	TCall  = "call"  // SDK call (ID, SDK, M, P)
	TSub   = "sub"   // SDK, Events
	TUnsub = "unsub" // SDK, Events
	TStats = "stats" // self-report of the script (P: ScriptStats)
	// script → init (not forwarded)
	TAlive = "alive" // event loop heartbeat (script watchdog)

	// engine ⇄ external provider (provider protocol over stdio; SID = session). They never reach a box:
	// FromScript and FromInit do not contain them.
	TInit   = "init"    // engine → provider (P: {proto, sdk, config})
	TInitOK = "init_ok" // provider → engine
	TOpen   = "open"    // engine → provider (SID, P: {box, script, grants})
	TOpened = "opened"  // provider → engine (SID, Err if rejected)
	TClose  = "close"   // engine → provider (SID)
	TEmit   = "emit"    // provider → engine (SID, Name, P)
)

var knownTypes = map[string]bool{
	TStart: true, TStop: true, TRet: true, TEvent: true, TPing: true, TPong: true,
	THello: true, TExit: true, TPStats: true,
	TReady: true, TLog: true, TCall: true, TSub: true, TUnsub: true, TStats: true, TAlive: true,
	TInit: true, TInitOK: true, TOpen: true, TOpened: true, TClose: true, TEmit: true,
}

// FromScript are the types a script process may send. Anything else is a protocol violation.
var FromScript = map[string]bool{
	TReady: true, TLog: true, TCall: true, TSub: true, TUnsub: true, TStats: true, TAlive: true,
}

// FromInit are the types the host accepts from init.
var FromInit = map[string]bool{
	THello: true, TExit: true, TPStats: true, TPong: true,
	TReady: true, TLog: true, TCall: true, TSub: true, TUnsub: true, TStats: true,
}

// Frame is deliberately flat so that Go and TypeScript agree without a schema generator.
type Frame struct {
	T      string          `json:"t"`
	SID    string          `json:"sid,omitempty"`    // script name (set by init from the pipe, never by the script)
	SIDs   []string        `json:"sids,omitempty"`   // recipients of an event (host → init only)
	ID     uint64          `json:"id,omitempty"`     // correlation of call/ret, ping/pong
	SDK    string          `json:"sdk,omitempty"`    // namespace of a call/sub/event
	M      string          `json:"m,omitempty"`      // method of a call
	Name   string          `json:"name,omitempty"`   // event name
	Events []string        `json:"events,omitempty"` // sub/unsub
	Level  string          `json:"level,omitempty"`  // log
	Msg    string          `json:"msg,omitempty"`    // log
	P      json.RawMessage `json:"p,omitempty"`      // type-specific payload
	Err    *Error          `json:"err,omitempty"`    // error of a ret
}

// Error is an error sent to the script. Code is machine-readable, Msg is for humans.
type Error struct {
	Code string `json:"code"`
	Msg  string `json:"msg,omitempty"`
}

// Hello is reported by init after it started.
type Hello struct {
	Proto   int    `json:"proto"`
	Init    string `json:"init"`    // version of tsbox-init
	UIDMode bool   `json:"uidMode"` // scripts run with their own UID
	// BaselineRSS is the VmRSS of an empty Deno process, if the system provides no RssAnon (gVisor).
	BaselineRSS int64 `json:"baselineRss,omitempty"`
}

// Start is the payload of start.
type Start struct {
	Artifact   string          `json:"artifact"` // path as seen by init
	SHA256     string          `json:"sha256"`   // init checks the artifact before every start
	UID        int             `json:"uid,omitempty"`
	HeapMiB    int             `json:"heapMiB"`
	RSSBytes   int64           `json:"rssBytes,omitempty"` // 0 = no per-script limit
	Threads    int             `json:"threads,omitempty"`  // 0 = no limit
	TimeoutMs  int64           `json:"timeoutMs,omitempty"`
	WatchdogMs int64           `json:"watchdogMs,omitempty"` // 0 = off
	Params     json.RawMessage `json:"params,omitempty"`     // not secret; ends up in argv
	// DebugExec replaces the Deno invocation. Only host tests set it (escape simulation).
	DebugExec []string `json:"debugExec,omitempty"`
}

// Exit reports the end of a script process.
type Exit struct {
	Code   int    `json:"code"`
	Signal string `json:"signal,omitempty"`
	// Reason: empty (regular), rss, threads, watchdog, timeout, integrity, protocol, backpressure,
	// spawn_failed, stopped, init_shutdown. The host assigns "oom".
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// ProcStats are measured by init for each script.
type ProcStats struct {
	SID     string `json:"sid"`
	RSS     int64  `json:"rss"`   // bytes, private (or VmRSS − baseline)
	CPUNs   int64  `json:"cpuNs"` // total CPU time
	Threads int    `json:"threads"`
}

// ScriptStats are reported by the script itself (untrusted, display only).
type ScriptStats struct {
	HeapUsed  int64 `json:"heapUsed"`
	HeapTotal int64 `json:"heapTotal"`
	External  int64 `json:"external"`
}

var (
	ErrTooLarge = errors.New("proto: frame too large")
	ErrEmpty    = errors.New("proto: empty frame")
)

// Encode encodes f including the length prefix.
func Encode(f *Frame) ([]byte, error) {
	b, err := json.Marshal(f)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxFrame {
		return nil, ErrTooLarge
	}
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	return buf, nil
}

// Write encodes f and writes it to w with a single Write call.
func Write(w io.Writer, f *Frame) error {
	buf, err := Encode(f)
	if err != nil {
		return err
	}
	_, err = w.Write(buf)
	return err
}

// Read reads exactly one frame. After an error the stream is out of sync and must be closed (except for
// io.EOF between frames).
func Read(r io.Reader) (*Frame, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	switch {
	case n == 0:
		return nil, ErrEmpty
	case n > MaxFrame:
		return nil, ErrTooLarge
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return Decode(b)
}

// Decode validates and decodes a frame body.
func Decode(b []byte) (*Frame, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var f Frame
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("proto: invalid JSON: %w", err)
	}
	if dec.More() {
		return nil, errors.New("proto: data after the frame")
	}
	if !knownTypes[f.T] {
		return nil, fmt.Errorf("proto: unknown type %q", f.T)
	}
	if f.ID > MaxID {
		return nil, errors.New("proto: id outside the JavaScript number range")
	}
	return &f, nil
}

// Payload strictly decodes f.P into v.
func Payload(f *Frame, v any) error {
	if len(f.P) == 0 {
		return errors.New("proto: payload missing")
	}
	dec := json.NewDecoder(bytes.NewReader(f.P))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("proto: payload of %s: %w", f.T, err)
	}
	return nil
}

// Raw encodes v as a payload (a programming error if that fails).
func Raw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
