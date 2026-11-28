package tsbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/rif223/tsbox/internal/proto"
	"github.com/rif223/tsbox/sdk"
)

// MaxEmitPayload is the largest payload Box.Emit accepts (JSON).
const MaxEmitPayload = 256 << 10

// ErrNotRunning: the box does not run, so there is no script to deliver to.
var ErrNotRunning = errors.New("box is not running")

// Emit delivers an event of an active SDK from the host to the scripts of the box that subscribed to it
// (@tsbox/core's on(sdk, event, …)); script "" means all scripts. It returns the number of scripts the
// event was queued for. Like events of providers, it never blocks: the event goes to all recipients as one
// frame through the box's queue to init, so with a full queue it is dropped for all of them, Emit returns 0
// and BoxStatus.DroppedEvents grows by one.
//
// The host is trusted (T0): Emit is for embedding applications and tsboxd, never reachable from a box. The
// usual channel is the bundled SDK inbox (sdk "inbox", event "message", payload {"topic": …, "data": …}).
func (b *Box) Emit(sdkName, event string, payload any, script string) (int, error) {
	raw, ok := payload.(json.RawMessage)
	if !ok {
		var err error
		if raw, err = json.Marshal(payload); err != nil {
			return 0, fmt.Errorf("payload: %w", err)
		}
	}
	if len(raw) == 0 {
		raw = json.RawMessage("null")
	}
	if !json.Valid(raw) {
		return 0, errors.New("payload is not valid JSON")
	}
	if len(raw) > MaxEmitPayload {
		return 0, fmt.Errorf("payload larger than %d KiB", MaxEmitPayload>>10)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	a := b.sdks[sdkName]
	if a == nil {
		return 0, fmt.Errorf("SDK %q is not active in box %q (--sdk %s)", sdkName, b.spec.Name, sdkName)
	}
	if _, ok := a.m.Events[event]; !ok {
		return 0, fmt.Errorf("SDK %q has no event %q", sdkName, event)
	}
	if script != "" {
		if _, ok := b.scripts[script]; !ok {
			return 0, fmt.Errorf("box %q has no script %q", b.spec.Name, script)
		}
	}
	if b.state != BoxRunning {
		return 0, ErrNotRunning
	}
	key := sdkName + ":" + event
	var sids []string
	for _, name := range b.order {
		if script != "" && name != script {
			continue
		}
		if s := b.scripts[name]; s.run != nil && s.run.subs[key] {
			sids = append(sids, name)
		}
	}
	if len(sids) == 0 {
		return 0, nil
	}
	f := &proto.Frame{T: proto.TEvent, SIDs: sids, SDK: sdkName, Name: event, P: raw}
	if !b.sendEventLocked(f) {
		return 0, nil
	}
	return len(sids), nil
}

// inboxProvider is the provider of @tsbox/inbox: send turns a script's message into an engine event.
type inboxProvider struct{ e *Engine }

var topicPattern = regexp.MustCompile(`^[\x21-\x7e]{1,64}$`)

func (p inboxProvider) Open(_ context.Context, s sdk.Session) (sdk.Handler, error) {
	r := sdk.NewRouter(s)
	sdk.Method(r, "send", func(_ context.Context, s sdk.Session, q struct {
		Topic string          `json:"topic"`
		Data  json.RawMessage `json:"data"`
	}) (any, error) {
		if !topicPattern.MatchString(q.Topic) {
			return nil, sdk.Errorf(sdk.InvalidArgument, "topic: 1-64 printable ASCII characters without spaces")
		}
		if len(q.Data) > 0 && string(q.Data) == "null" {
			q.Data = nil
		}
		p.e.emit(Event{At: time.Now(), Kind: EventScriptMessage, Box: s.Box().Name, Script: s.Script().Name,
			Detail: q.Topic, Payload: append(json.RawMessage(nil), q.Data...)})
		return map[string]any{}, nil
	})
	return r, nil
}
