package daemon

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rif223/tsbox"
)

// hMetrics writes measurements in the Prometheus text format (GET /v1/metrics, and GET /metrics on
// --metrics-addr). It contains names, states and numbers, never specifications or secrets.
func (d *Daemon) hMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	bw := bufio.NewWriter(w)
	d.writeMetrics(ctx, bw)
	_ = bw.Flush()
}

type metricsWriter struct {
	w    io.Writer
	seen map[string]bool
}

func (m *metricsWriter) head(name, typ, help string) {
	if m.seen[name] {
		return
	}
	m.seen[name] = true
	fmt.Fprintf(m.w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func (m *metricsWriter) val(name string, v float64, labels ...string) {
	var b strings.Builder
	b.WriteString(name)
	if len(labels) > 0 {
		b.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(labels[i])
			b.WriteString(`="`)
			b.WriteString(escapeLabel(labels[i+1]))
			b.WriteByte('"')
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(strconv.FormatFloat(v, 'g', -1, 64))
	b.WriteByte('\n')
	_, _ = io.WriteString(m.w, b.String())
}

func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func (d *Daemon) writeMetrics(ctx context.Context, w io.Writer) {
	m := &metricsWriter{w: w, seen: map[string]bool{}}
	m.head("tsbox_info", "gauge", "Version of tsboxd.")
	m.val("tsbox_info", 1, "version", tsbox.Version)

	states := map[tsbox.BoxState]int{}
	type loaded struct {
		name string
		b    *tsbox.Box
	}
	var boxes []loaded
	for _, rec := range d.records() {
		v := rec.view(d)
		states[v.State]++
		rec.mu.Lock()
		b := rec.box
		rec.mu.Unlock()
		if b != nil {
			boxes = append(boxes, loaded{rec.name, b})
		}
	}
	m.head("tsbox_boxes", "gauge", "Boxes known to tsboxd by state.")
	keys := make([]string, 0, len(states))
	for s := range states {
		keys = append(keys, string(s))
	}
	sort.Strings(keys)
	for _, s := range keys {
		m.val("tsbox_boxes", float64(states[tsbox.BoxState(s)]), "state", s)
	}
	m.head("tsbox_dropped_total", "counter", "Engine events and log lines dropped because of a full queue.")
	m.val("tsbox_dropped_total", float64(d.e.DroppedEvents()))

	for _, lb := range boxes {
		st := lb.b.Status()
		box := lb.name
		up := 0.0
		if !terminal(st.State) {
			up = 1
		}
		m.head("tsbox_box_up", "gauge", "1 if the box is starting, running or stopping.")
		m.val("tsbox_box_up", up, "box", box, "driver", st.Driver)
		m.head("tsbox_box_memory_limit_bytes", "gauge", "Memory limit of the box.")
		m.val("tsbox_box_memory_limit_bytes", float64(st.Limits.Memory), "box", box)
		m.head("tsbox_box_cpu_limit_cores", "gauge", "CPU limit of the box.")
		m.val("tsbox_box_cpu_limit_cores", st.Limits.CPU, "box", box)
		m.head("tsbox_box_pids_limit", "gauge", "PID limit of the box.")
		m.val("tsbox_box_pids_limit", float64(st.Limits.PIDs), "box", box)
		m.head("tsbox_box_dropped_events_total", "counter", "SDK events dropped for the box (full queue).")
		m.val("tsbox_box_dropped_events_total", float64(st.DroppedEvents), "box", box)
		for _, sc := range st.Scripts {
			sup := 0.0
			switch sc.State {
			case tsbox.ScriptStarting, tsbox.ScriptRunning, tsbox.ScriptReady:
				sup = 1
			}
			m.head("tsbox_script_up", "gauge", "1 if the script is starting, running or ready.")
			m.val("tsbox_script_up", sup, "box", box, "script", sc.Name, "state", string(sc.State))
			m.head("tsbox_script_restarts_total", "counter", "Restarts of the script since the box started.")
			m.val("tsbox_script_restarts_total", float64(sc.Restarts), "box", box, "script", sc.Name)
			m.head("tsbox_script_log_lines_dropped_total", "counter", "Log lines over the rate limit (20/s, burst 100).")
			m.val("tsbox_script_log_lines_dropped_total", float64(sc.DroppedLogLines), "box", box, "script", sc.Name)
		}
		if up == 0 {
			continue
		}
		sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		bs, err := lb.b.Stats(sctx)
		cancel()
		if err != nil {
			continue
		}
		m.head("tsbox_box_memory_bytes", "gauge", "Memory used by the box.")
		m.val("tsbox_box_memory_bytes", float64(bs.Memory), "box", box)
		m.head("tsbox_box_memory_peak_bytes", "gauge", "Peak memory of the box.")
		m.val("tsbox_box_memory_peak_bytes", float64(bs.MemoryPeak), "box", box)
		m.head("tsbox_box_cpu_cores", "gauge", "CPU used by the box (cores).")
		m.val("tsbox_box_cpu_cores", bs.CPUCores, "box", box)
		m.head("tsbox_box_pids", "gauge", "Processes and threads of the box.")
		m.val("tsbox_box_pids", float64(bs.PIDs), "box", box)
		m.head("tsbox_box_oom_kills_total", "counter", "Kernel OOM kills in the box.")
		m.val("tsbox_box_oom_kills_total", float64(bs.OOMKills), "box", box)
		for _, sc := range bs.Scripts {
			m.head("tsbox_script_rss_bytes", "gauge", "Private memory of the script.")
			m.val("tsbox_script_rss_bytes", float64(sc.RSS), "box", box, "script", sc.Name)
			m.head("tsbox_script_heap_used_bytes", "gauge", "V8 heap used by the script.")
			m.val("tsbox_script_heap_used_bytes", float64(sc.HeapUsed), "box", box, "script", sc.Name)
			m.head("tsbox_script_cpu_cores", "gauge", "CPU used by the script (cores).")
			m.val("tsbox_script_cpu_cores", sc.CPUCores, "box", box, "script", sc.Name)
			m.head("tsbox_script_threads", "gauge", "Threads of the script.")
			m.val("tsbox_script_threads", float64(sc.Threads), "box", box, "script", sc.Name)
		}
	}
	for _, c := range d.e.SDKCalls() {
		m.head("tsbox_sdk_calls_total", "counter", "Finished SDK calls by result (ok or error code).")
		m.val("tsbox_sdk_calls_total", float64(c.Count), "box", c.Box, "sdk", c.SDK, "method", c.Method, "code", c.Code)
	}
	for _, t := range d.e.Tenants() {
		m.head("tsbox_tenant_boxes", "gauge", "Boxes of the tenant (engine-loaded).")
		m.val("tsbox_tenant_boxes", float64(t.Boxes), "tenant", t.Tenant)
		m.head("tsbox_tenant_running", "gauge", "Running boxes of the tenant.")
		m.val("tsbox_tenant_running", float64(t.Running), "tenant", t.Tenant)
		m.head("tsbox_tenant_memory_limit_bytes", "gauge", "Sum of the memory limits of the tenant's running boxes.")
		m.val("tsbox_tenant_memory_limit_bytes", float64(t.Limits.Memory), "tenant", t.Tenant)
		m.head("tsbox_tenant_cpu_limit_cores", "gauge", "Sum of the CPU limits of the tenant's running boxes.")
		m.val("tsbox_tenant_cpu_limit_cores", t.Limits.CPU, "tenant", t.Tenant)
		if t.Quota.Memory > 0 {
			m.head("tsbox_tenant_quota_memory_bytes", "gauge", "Memory quota of the tenant.")
			m.val("tsbox_tenant_quota_memory_bytes", float64(t.Quota.Memory), "tenant", t.Tenant)
		}
		if t.Quota.CPU > 0 {
			m.head("tsbox_tenant_quota_cpu_cores", "gauge", "CPU quota of the tenant.")
			m.val("tsbox_tenant_quota_cpu_cores", t.Quota.CPU, "tenant", t.Tenant)
		}
	}
}

// serveMetrics serves only GET /metrics on a TCP address (opt-in: --metrics-addr). Everything else needs
// the socket.
func (d *Daemon) serveMetrics(ctx context.Context, addr string) (func(), error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("metrics address %s: %w", addr, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", d.hMetrics)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() { _ = srv.Serve(ln) }()
	d.log.Info("metrics", "addr", ln.Addr().String())
	return func() {
		sctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}, nil
}
