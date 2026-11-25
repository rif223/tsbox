package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime/pprof"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/rif223/tsbox"
	"github.com/rif223/tsbox/client"
)

const maxBody = 4 << 20

func (d *Daemon) routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /v1/info", d.quick(d.hInfo))
	m.HandleFunc("GET /v1/boxes", d.quick(d.hList))
	m.HandleFunc("POST /v1/boxes", d.hCreate)
	m.HandleFunc("GET /v1/boxes/{name}", d.quick(d.hInspect))
	m.HandleFunc("POST /v1/boxes/{name}/start", d.hStart)
	m.HandleFunc("POST /v1/boxes/{name}/stop", d.hStop)
	m.HandleFunc("POST /v1/boxes/{name}/restart", d.hRestart)
	m.HandleFunc("DELETE /v1/boxes/{name}", d.hRemove)
	m.HandleFunc("GET /v1/boxes/{name}/logs", d.hLogs)
	m.HandleFunc("GET /v1/stats", d.hStats)
	m.HandleFunc("GET /v1/events", d.hEvents)
	m.HandleFunc("POST /v1/sdks/{name}/release", d.quick(d.hReleaseSDK))
	m.HandleFunc("POST /v1/shutdown", d.quick(d.hShutdown))
	return m
}

// quick: calls without a stream must answer immediately. If one hangs for 20 s, tsboxd writes all
// goroutines to the log once (diagnostics for blocked locks).
func (d *Daemon) quick(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t := time.AfterFunc(20*time.Second, func() {
			d.dumpOnce.Do(func() {
				d.log.Error("API call hangs for 20 s, goroutines follow", "path", r.URL.Path)
				_ = pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
			})
		})
		defer t.Stop()
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var ae *client.Error
	if !errors.As(err, &ae) {
		ae = &client.Error{Status: 500, Code: "internal", Message: err.Error()}
	}
	writeJSON(w, ae.Status, map[string]any{"error": ae})
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &client.Error{Status: 400, Code: "invalid", Message: "request: " + err.Error()}
	}
	return nil
}

func queryTime(r *http.Request, key string) (time.Time, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return t, &client.Error{Status: 400, Code: "invalid", Message: key + ": " + err.Error()}
	}
	return t, nil
}

func queryDuration(r *http.Request, key string) (time.Duration, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return 0, nil
	}
	t, err := time.ParseDuration(v)
	if err != nil {
		return t, &client.Error{Status: 400, Code: "invalid", Message: key + ": " + err.Error()}
	}
	return t, nil
}

// ndjson prepares a streaming response; send writes one element and flushes the buffer.
func ndjson(w http.ResponseWriter) func(any) error {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	if fl != nil {
		fl.Flush()
	}
	enc := json.NewEncoder(w)
	var mu sync.Mutex
	return func(v any) error {
		mu.Lock()
		defer mu.Unlock()
		if err := enc.Encode(v); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}
}

func (d *Daemon) hInfo(w http.ResponseWriter, _ *http.Request) {
	mode := "user"
	if d.cfg.System {
		mode = "system"
	}
	idle := ""
	if d.cfg.Idle > 0 {
		idle = d.cfg.Idle.String()
	}
	writeJSON(w, 200, client.Info{Version: tsbox.Version, API: client.APIVersion, Build: d.build, PID: os.Getpid(),
		Mode: mode, Autostarted: d.cfg.Autostarted, Idle: idle,
		Drivers: d.e.Drivers(), StateDir: d.cfg.StateDir, Socket: d.cfg.Socket, Running: d.running(), Boxes: len(d.records())})
}

func (d *Daemon) hList(w http.ResponseWriter, r *http.Request) {
	all := r.URL.Query().Get("all") == "true"
	out := []client.Box{}
	for _, rec := range d.records() {
		v := rec.view(d)
		if all || !terminal(v.State) {
			out = append(out, v)
		}
	}
	writeJSON(w, 200, out)
}

func (d *Daemon) hCreate(w http.ResponseWriter, r *http.Request) {
	var req client.CreateRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.Attach {
		d.attach(w, r, req)
		return
	}
	rec, err := d.create(r.Context(), req, false)
	if err != nil {
		writeErr(w, err)
		return
	}
	if req.Start {
		if err := d.start(r.Context(), rec, nil); err != nil {
			writeErr(w, err)
			return
		}
	}
	writeJSON(w, 201, rec.view(d))
}

// attach: create, start and stream logs, events and the final status. If the client leaves before, the
// box is stopped (and removed with AutoRemove).
func (d *Daemon) attach(w http.ResponseWriter, r *http.Request, req client.CreateRequest) {
	send := ndjson(w)
	ctx, cancel := context.WithCancel(r.Context())
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	name := req.Spec.Name
	_, evs, unsub := d.subscribeEvents(time.Time{})
	defer unsub()

	rec, err := d.create(ctx, req, true)
	if err != nil {
		var ae *client.Error
		if !errors.As(err, &ae) {
			ae = &client.Error{Status: 500, Code: "internal", Message: err.Error()}
		}
		_ = send(client.AttachMsg{Type: "error", Error: ae})
		return
	}
	v := rec.view(d)
	_ = send(client.AttachMsg{Type: "created", Box: &v})
	rec.mu.Lock()
	b := rec.box
	rec.mu.Unlock()

	logsDone := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer close(logsDone)
		for l, err := range b.Logs(ctx, tsbox.LogOptions{Follow: true}) {
			if err != nil || send(client.AttachMsg{Type: "log", Log: &l}) != nil {
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-evs:
				if ev.Box == name {
					_ = send(client.AttachMsg{Type: "event", Event: &ev})
				}
			}
		}
	}()

	cleanup := func() {
		bg, c := context.WithTimeout(context.Background(), 30*time.Second)
		defer c()
		_ = d.stopBox(bg, rec, 3*time.Second)
		if req.AutoRemove {
			_ = d.remove(bg, name, true)
		} else {
			rec.mu.Lock()
			rec.attached = false
			rec.mu.Unlock()
		}
	}
	if err := d.start(ctx, rec, nil); err != nil {
		var ae *client.Error
		if !errors.As(err, &ae) {
			ae = &client.Error{Status: 500, Code: "internal", Message: err.Error()}
		}
		_ = send(client.AttachMsg{Type: "error", Error: ae})
		cleanup()
		return
	}
	st, err := b.Wait(ctx)
	if err != nil { // client gone
		cleanup()
		return
	}
	select {
	case <-logsDone:
	case <-time.After(2 * time.Second):
	}
	_ = send(client.AttachMsg{Type: "exit", Status: &st})
	if req.AutoRemove {
		_ = d.remove(context.Background(), name, true)
	} else {
		rec.mu.Lock()
		rec.attached = false
		rec.mu.Unlock()
	}
}

func (d *Daemon) hInspect(w http.ResponseWriter, r *http.Request) {
	rec, err := d.get(r.PathValue("name"))
	if err != nil {
		writeErr(w, err)
		return
	}
	v := rec.view(d)
	rec.mu.Lock()
	out := client.Inspect{Box: v, Spec: rec.spec, Dir: d.boxDir(rec.name), LogDir: filepath.Join(d.boxDir(rec.name), "logs")}
	for k := range rec.env {
		out.EnvSecrets = append(out.EnvSecrets, k)
	}
	rec.mu.Unlock()
	sort.Strings(out.EnvSecrets)
	writeJSON(w, 200, out)
}

func (d *Daemon) hStart(w http.ResponseWriter, r *http.Request) {
	rec, err := d.get(r.PathValue("name"))
	if err != nil {
		writeErr(w, err)
		return
	}
	var body struct {
		Env map[string]string `json:"env"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &body); err != nil {
			writeErr(w, err)
			return
		}
	}
	if err := d.start(r.Context(), rec, body.Env); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, rec.view(d))
}

// stopRequested stops the box named in the request with the requested grace period.
func (d *Daemon) stopRequested(r *http.Request) (*record, error) {
	rec, err := d.get(r.PathValue("name"))
	if err != nil {
		return nil, err
	}
	grace, err := queryDuration(r, "grace")
	if err != nil {
		return nil, err
	}
	if err := d.stopBox(r.Context(), rec, grace); err != nil {
		return nil, err
	}
	return rec, nil
}

func (d *Daemon) hStop(w http.ResponseWriter, r *http.Request) {
	rec, err := d.stopRequested(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, rec.view(d))
}

func (d *Daemon) hRestart(w http.ResponseWriter, r *http.Request) {
	rec, err := d.stopRequested(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := d.start(r.Context(), rec, nil); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, rec.view(d))
}

func (d *Daemon) hRemove(w http.ResponseWriter, r *http.Request) {
	if err := d.remove(r.Context(), r.PathValue("name"), r.URL.Query().Get("force") == "true"); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d *Daemon) hLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := d.get(name); err != nil {
		writeErr(w, err)
		return
	}
	since, err := queryTime(r, "since")
	if err != nil {
		writeErr(w, err)
		return
	}
	q := r.URL.Query()
	tail, _ := strconv.Atoi(q.Get("tail"))
	o := tsbox.LogOptions{Since: since, Script: q.Get("script"), Tail: tail, Follow: q.Get("follow") == "true"}
	send := ndjson(w)
	for l, err := range d.e.Logs(r.Context(), name, o) {
		if err != nil {
			return
		}
		if send(l) != nil {
			return
		}
	}
}

func (d *Daemon) stats(ctx context.Context, only string) []client.StatsEntry {
	out := []client.StatsEntry{}
	for _, rec := range d.records() {
		if only != "" && rec.name != only {
			continue
		}
		rec.mu.Lock()
		b, spec := rec.box, rec.spec
		rec.mu.Unlock()
		if b == nil {
			continue
		}
		st := b.Status()
		if terminal(st.State) && only == "" {
			continue
		}
		e := client.StatsEntry{Box: rec.name, State: st.State, Started: st.Started, Limits: st.Limits, Scripts: st.Scripts}
		for _, sc := range spec.Scripts {
			if sc.Limits.RSS > 0 {
				if e.RSSLimits == nil {
					e.RSSLimits = map[string]int64{}
				}
				e.RSSLimits[sc.Name] = sc.Limits.RSS
			}
		}
		if !terminal(st.State) {
			sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			s, err := b.Stats(sctx)
			cancel()
			e.Stats = s
			if err != nil {
				e.Error = err.Error()
			}
		}
		out = append(out, e)
	}
	return out
}

func (d *Daemon) hStats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	only := q.Get("box")
	if only != "" {
		if _, err := d.get(only); err != nil {
			writeErr(w, err)
			return
		}
	}
	send := ndjson(w)
	if send(d.stats(r.Context(), only)) != nil || q.Get("stream") != "true" {
		return
	}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
			if send(d.stats(r.Context(), only)) != nil {
				return
			}
		}
	}
}

func (d *Daemon) hEvents(w http.ResponseWriter, r *http.Request) {
	since, err := queryTime(r, "since")
	if err != nil {
		writeErr(w, err)
		return
	}
	box := r.URL.Query().Get("box")
	past, ch, unsub := d.subscribeEvents(since)
	defer unsub()
	send := ndjson(w)
	for _, ev := range past {
		if box == "" || ev.Box == box {
			if send(ev) != nil {
				return
			}
		}
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			if box == "" || ev.Box == box {
				if send(ev) != nil {
					return
				}
			}
		}
	}
}

// hReleaseSDK ends the provider processes of an installed SDK before tsbox sdk add/rm changes its files.
func (d *Daemon) hReleaseSDK(w http.ResponseWriter, r *http.Request) {
	if err := d.e.ReleaseSDK(r.PathValue("name")); err != nil {
		if errors.Is(err, tsbox.ErrSDKInUse) {
			err = &client.Error{Status: 409, Code: "in_use", Message: err.Error()}
		}
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d *Daemon) hShutdown(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
	d.Shutdown()
}
