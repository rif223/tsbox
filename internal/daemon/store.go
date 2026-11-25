package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/rif223/tsbox"
	"github.com/rif223/tsbox/internal/fsutil"
)

// stateFile lives in every box's directory (<state>/boxes/<name>/tsboxd.json). It contains only the
// specification with references (file:/env:), never secret values.
const stateFile = "tsboxd.json"

type persisted struct {
	Spec       tsbox.BoxSpec    `json:"spec"`
	Created    time.Time        `json:"created"`
	Desired    string           `json:"desired"`
	AutoRemove bool             `json:"autoRemove,omitempty"`
	Last       *tsbox.BoxStatus `json:"last,omitempty"`
}

func (d *Daemon) boxDir(name string) string { return filepath.Join(d.cfg.StateDir, "boxes", name) }

// save writes the state of a box atomically (tmp + rename). The caller holds rec.mu.
func (d *Daemon) save(r *record) {
	if r.removed {
		return
	}
	p := persisted{Spec: r.spec, Created: r.created, Desired: r.desired, AutoRemove: r.autoRemove, Last: r.last}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		d.log.Error("state", "box", r.name, "err", err)
		return
	}
	dir := d.boxDir(r.name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		d.log.Error("state", "box", r.name, "err", err)
		return
	}
	tmp := filepath.Join(dir, stateFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		d.log.Error("state", "box", r.name, "err", err)
		return
	}
	if err := fsutil.Rename(tmp, filepath.Join(dir, stateFile)); err != nil {
		d.log.Error("state", "box", r.name, "err", err)
	}
}

// load reads all stored boxes.
func (d *Daemon) load() []*record {
	ents, _ := os.ReadDir(filepath.Join(d.cfg.StateDir, "boxes"))
	var out []*record
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(d.boxDir(e.Name()), stateFile))
		if err != nil {
			continue // box without daemon state (e.g. from the Go API)
		}
		var p persisted
		if err := json.Unmarshal(b, &p); err != nil || p.Spec.Name != e.Name() {
			d.log.Warn("state unreadable, skipped", "box", e.Name(), "err", err)
			continue
		}
		r := &record{name: p.Spec.Name, spec: p.Spec, created: p.Created, desired: p.Desired, autoRemove: p.AutoRemove, last: p.Last}
		// Boxes stored by tsbox 0.2 may lack the driver in their specification (it was the daemon's default);
		// the driver of their last run is the one they had.
		if r.spec.Driver == "" && r.last != nil && r.last.Driver != "" {
			r.spec.Driver = r.last.Driver
			d.log.Info("driver of the last run taken over", "box", r.name, "driver", r.spec.Driver)
		}
		if r.last != nil {
			switch r.last.State {
			case tsbox.BoxExited, tsbox.BoxFailed, tsbox.BoxQuarantined, tsbox.BoxCreated:
			default: // the daemon ended while the box was running
				r.last.State = tsbox.BoxExited
				if r.last.Error == "" {
					r.last.Error = "tsboxd was stopped"
				}
			}
		}
		out = append(out, r)
	}
	return out
}
