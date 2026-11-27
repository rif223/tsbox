// Package sdk is the Go interface for providers: the host side of an SDK, which executes a script's
// calls. Before every call the core has already checked: SDK activated, method in the manifest, right
// granted, parameter size, rate, concurrency; it also enforces the time limit and the result size. The
// provider checks the content (e.g. whether a host really matches the grant).
//
// Identity (box, script) always comes from the core, never from the parameters.
package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// Provider creates one handler per script instance.
type Provider interface {
	Open(ctx context.Context, s Session) (Handler, error)
}

// Handler processes a script's calls. Call runs concurrently (up to the manifest's concurrency); ctx
// ends with the method's time limit or with the end of the script.
type Handler interface {
	Call(ctx context.Context, method string, params json.RawMessage) (any, error)
	Close() error
}

// Session is the provider's view of a script.
type Session interface {
	Box() BoxRef
	Script() ScriptRef
	Grants() Grants
	Config() Config
	// Emit delivers an event if the script has subscribed to it. Never blocks; ErrQueueFull if the queue
	// is full (counted), ErrUnknownEvent if the manifest does not know the event.
	Emit(event string, payload any) error
	// ScriptLog writes to the script's log (with its rate limit).
	ScriptLog(level, msg string)
	// Logger is for messages of the provider itself (host log).
	Logger() *slog.Logger
}

// BoxRef and ScriptRef describe the caller.
type BoxRef struct {
	Name    string            `json:"name"`
	Storage string            `json:"storage"` // storage area for stateful SDKs
	Labels  map[string]string `json:"labels,omitempty"`
}

type ScriptRef struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// Grants are the granted rights (right → values).
type Grants map[string][]string

func (g Grants) Values(perm string) []string { return g[perm] }

func (g Grants) Value(perm string) string {
	if v := g[perm]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// Config is the resolved operator configuration (with defaults, secrets resolved).
type Config map[string]string

func (c Config) String(k string) string { return c[k] }

func (c Config) Bool(k string) bool {
	v, _ := strconv.ParseBool(c[k])
	return v
}

func (c Config) Int(k string) int64 {
	v, _ := strconv.ParseInt(c[k], 10, 64)
	return v
}

func (c Config) Duration(k string) time.Duration {
	v, _ := time.ParseDuration(c[k])
	return v
}

// Bytes understands 64Ki, 1Mi … (as in the manifest).
func (c Config) Bytes(k string) int64 {
	s := c[k]
	for _, u := range []struct {
		suf string
		m   int64
	}{{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}} {
		if strings.HasSuffix(s, u.suf) {
			v, _ := strconv.ParseFloat(strings.TrimSuffix(s, u.suf), 64)
			return int64(v * float64(u.m))
		}
	}
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

// Error codes sent to the script.
const (
	InvalidArgument  = "invalid_argument"
	PermissionDenied = "permission_denied"
	NotFound         = "not_found"
	RateLimited      = "rate_limited"
	TooLarge         = "too_large"
	Timeout          = "timeout"
	Unavailable      = "unavailable"
	QuotaExceeded    = "quota_exceeded"
	Internal         = "internal"
)

// Error is an error the script sees (code + message). The core reports other errors as a neutral
// "internal"; their details are only in the host log.
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return e.Code + ": " + e.Msg }

// Errorf creates an error for the script.
func Errorf(code, format string, a ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, a...)}
}

var (
	ErrQueueFull    = errors.New("sdk: event queue full")
	ErrUnknownEvent = errors.New("sdk: event not in the manifest")
	ErrNotSubscribe = errors.New("sdk: event not subscribed")
)

// Router is a handler made of typed methods.
type Router struct {
	s       Session
	methods map[string]func(context.Context, json.RawMessage) (any, error)
	close   func() error
}

// NewRouter creates a router for a session.
func NewRouter(s Session) *Router {
	return &Router{s: s, methods: map[string]func(context.Context, json.RawMessage) (any, error){}}
}

// OnClose sets a cleanup function.
func (r *Router) OnClose(fn func() error) { r.close = fn }

// Method registers a method. Parameters are decoded strictly (unknown fields ⇒ invalid_argument).
func Method[Req, Res any](r *Router, name string, fn func(ctx context.Context, s Session, req Req) (Res, error)) {
	r.methods[name] = func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req Req
		if len(raw) > 0 && string(raw) != "null" {
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&req); err != nil {
				return nil, Errorf(InvalidArgument, "parameters: %v", err)
			}
		}
		return fn(ctx, r.s, req)
	}
}

func (r *Router) Call(ctx context.Context, method string, params json.RawMessage) (any, error) {
	fn, ok := r.methods[method]
	if !ok {
		return nil, Errorf(NotFound, "method %q unknown", method)
	}
	return fn(ctx, params)
}

func (r *Router) Close() error {
	if r.close != nil {
		return r.close()
	}
	return nil
}

// ProviderFunc turns a function into a provider.
type ProviderFunc func(ctx context.Context, s Session) (Handler, error)

func (f ProviderFunc) Open(ctx context.Context, s Session) (Handler, error) { return f(ctx, s) }

// GrantValidator is an optional interface of providers: the engine calls ValidateGrants when a box is
// created or checked, so that a grant the provider cannot use (e.g. an invalid schedule) is refused at
// once instead of at the first call.
type GrantValidator interface {
	ValidateGrants(g Grants, c Config) error
}
