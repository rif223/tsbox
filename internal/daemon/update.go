package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/rif223/tsbox"
	"github.com/rif223/tsbox/client"
	"github.com/rif223/tsbox/internal/build"
)

// hUpdate replaces the specification of a box (PUT /v1/boxes/{name}): build and check first, then stop the
// old box and start the new one if it was running. Name, storage and log files stay.
func (d *Daemon) hUpdate(w http.ResponseWriter, r *http.Request) {
	var req client.UpdateRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	name := r.PathValue("name")
	if req.Spec.Name == "" {
		req.Spec.Name = name
	}
	if req.Spec.Name != name {
		writeErr(w, &client.Error{Status: 400, Code: "invalid", Message: fmt.Sprintf("spec.name %q differs from the box %q (renaming is not supported)", req.Spec.Name, name)})
		return
	}
	var grace time.Duration
	if req.Grace != "" {
		g, err := time.ParseDuration(req.Grace)
		if err != nil || g < 0 {
			writeErr(w, &client.Error{Status: 400, Code: "invalid", Message: "grace: " + req.Grace})
			return
		}
		grace = g
	}
	rec, err := d.get(name)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := d.update(r.Context(), rec, req.Spec, req.Env, grace); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, rec.view(d))
}

func (d *Daemon) update(ctx context.Context, r *record, spec tsbox.BoxSpec, env map[string]string, grace time.Duration) error {
	r.op.Lock()
	defer r.op.Unlock()
	d.busy.Add(1)
	defer d.busy.Add(-1)
	r.mu.Lock()
	if r.removed {
		r.mu.Unlock()
		return &client.Error{Status: 404, Code: "not_found", Message: fmt.Sprintf("box %q not found", r.name)}
	}
	if r.attached {
		r.mu.Unlock()
		return &client.Error{Status: 409, Code: "conflict", Message: fmt.Sprintf("box %q runs attached to a terminal (tsbox run without -d)", r.name)}
	}
	merged := map[string]string{}
	for k, v := range r.env {
		merged[k] = v
	}
	for k, v := range env {
		merged[k] = v
	}
	b := r.box
	r.updating = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.updating = false
		r.mu.Unlock()
	}()

	full := withEnv(spec, merged)
	var nb *tsbox.Box
	var err error
	if b == nil {
		// not loaded (e.g. after a daemon restart): check, then only the stored specification changes
		_, err = d.e.Check(ctx, full)
	} else {
		nb, err = d.e.Update(ctx, full, grace)
	}
	if err != nil {
		if b != nil {
			d.afterFailedUpdate(r, b)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var be *build.Error
		if errors.As(err, &be) {
			return &client.Error{Status: 400, Code: "build", Message: be.Error()}
		}
		if errors.Is(err, tsbox.ErrQuotaExceeded) {
			return &client.Error{Status: 409, Code: "quota", Message: err.Error()}
		}
		return invalid(err)
	}
	r.mu.Lock()
	r.env = merged
	if nb != nil {
		r.box, r.spec = nb, nb.Spec()
		st := nb.Status()
		r.last = &st
	} else {
		r.spec = tsbox.BoxSpec(stripEnv(full))
	}
	d.save(r)
	running := nb != nil && !terminal(nb.Status().State)
	r.mu.Unlock()
	if running {
		d.wg.Add(1)
		go d.watch(r, nb)
	}
	d.onEvent(tsbox.Event{At: time.Now(), Kind: client.EventBoxUpdated, Box: r.name})
	return nil
}

// afterFailedUpdate brings the record in line with the engine after a failed Engine.Update. If the old box
// was stopped, the engine either runs a rollback box (a new *tsbox.Box with the old specification) or knows
// no box of this name any more; the watcher of the old box ignored its end (updating). Without this, the
// record kept pointing at the stopped old box: ps showed it stopped while the rollback box ran unwatched,
// and stop/rm did not reach it.
func (d *Daemon) afterFailedUpdate(r *record, old *tsbox.Box) {
	cur, ok := d.e.Box(r.name)
	r.mu.Lock()
	r.updating = false // the watcher started below must record the end of the rollback box
	switch {
	case ok && cur != old: // rolled back
		r.box = cur
	case !ok: // neither the new nor the old specification runs; start creates the box anew
		r.box = nil
	case !terminal(old.Status().State): // the old box was not touched (build, check or quota error)
		r.mu.Unlock()
		return
	}
	st := old.Status()
	if cur != nil && ok {
		st = cur.Status()
	}
	r.last = &st
	d.save(r)
	running := r.box != nil && !terminal(st.State)
	r.mu.Unlock()
	if running {
		d.wg.Add(1)
		go d.watch(r, cur)
	}
}

func stripEnv(s tsbox.BoxSpec) tsbox.BoxSpec {
	s.SDKs = append([]tsbox.SDKUse(nil), s.SDKs...)
	for i := range s.SDKs {
		s.SDKs[i].Env = nil
	}
	return s
}

// hEmit delivers an event from the caller to the scripts of a running box (POST /v1/boxes/{name}/emit).
func (d *Daemon) hEmit(w http.ResponseWriter, r *http.Request) {
	var req client.EmitRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	rec, err := d.get(r.PathValue("name"))
	if err != nil {
		writeErr(w, err)
		return
	}
	rec.mu.Lock()
	b := rec.box
	rec.mu.Unlock()
	if b == nil {
		writeErr(w, &client.Error{Status: 409, Code: "not_running", Message: fmt.Sprintf("box %q is not running", rec.name)})
		return
	}
	if req.SDK == "" {
		req.SDK = "inbox"
	}
	if req.Event == "" {
		req.Event = "message"
	}
	payload := req.Payload
	if len(payload) == 0 {
		payload = json.RawMessage("null")
	}
	n, err := b.Emit(req.SDK, req.Event, payload, req.Script)
	if err != nil {
		if errors.Is(err, tsbox.ErrNotRunning) {
			writeErr(w, &client.Error{Status: 409, Code: "not_running", Message: fmt.Sprintf("box %q is not running", rec.name)})
			return
		}
		writeErr(w, &client.Error{Status: 400, Code: "invalid", Message: err.Error()})
		return
	}
	writeJSON(w, 200, client.EmitResult{Delivered: n})
}
