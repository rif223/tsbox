package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/rif223/tsbox"
	"github.com/rif223/tsbox/kvstore"
)

// SystemSocket is the socket of the system daemon (Linux, systemd, group tsbox).
const SystemSocket = "/run/tsbox/tsboxd.sock"

// Hosts names the addresses the CLI tries in order: TSBOX_HOST, otherwise on Linux the system socket
// (only without TSBOX_STATE) and the address of the user daemon (UserHost).
func Hosts() []string {
	if h := os.Getenv("TSBOX_HOST"); h != "" {
		return []string{h}
	}
	var out []string
	if runtime.GOOS == "linux" && os.Getenv("TSBOX_STATE") == "" {
		out = append(out, "unix://"+SystemSocket)
	}
	return append(out, UserHost(tsbox.DefaultStateDir()))
}

// SocketPath returns the path from "unix://<path>", "npipe://<path>" (Windows; also in Docker notation
// npipe:////./pipe/name) or a plain path.
func SocketPath(host string) (string, error) {
	if p, ok := strings.CutPrefix(host, "unix://"); ok {
		return p, nil
	}
	if p, ok := strings.CutPrefix(host, "npipe://"); ok {
		return strings.ReplaceAll(p, "/", `\`), nil
	}
	if strings.Contains(host, "://") {
		return "", fmt.Errorf("address %q: supported are unix://<path> and npipe://<path>", host)
	}
	return host, nil
}

// Client talks to a daemon.
type Client struct {
	host string
	path string
	hc   *http.Client
}

// New creates a client; it connects on the first request.
func New(host string) (*Client, error) {
	p, err := SocketPath(host)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dial(ctx, p)
		},
		MaxIdleConns:    4,
		IdleConnTimeout: 5 * time.Second,
	}
	return &Client{host: host, path: p, hc: &http.Client{Transport: tr}}, nil
}

// Host is the address of the daemon.
func (c *Client) Host() string { return c.host }

// Close releases open connections (otherwise the daemon counts them until the idle timeout).
func (c *Client) Close() { c.hc.CloseIdleConnections() }

func (c *Client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://tsboxd/v1"+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		var e struct {
			Error *Error `json:"error"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if json.Unmarshal(b, &e) != nil || e.Error == nil {
			return nil, &Error{Status: resp.StatusCode, Code: "internal", Message: fmt.Sprintf("tsboxd: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))}
		}
		e.Error.Status = resp.StatusCode
		return nil, e.Error
	}
	return resp, nil
}

func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	resp, err := c.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ndjson reads JSON lines (at most 8 MiB per line) and calls fn for each one until fn returns false.
// decodeErr reports an invalid line, readErr a read error.
func ndjson[T any](r io.Reader, fn func(T) bool) (decodeErr, readErr error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		var v T
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			return err, nil
		}
		if !fn(v) {
			return nil, nil
		}
	}
	return nil, sc.Err()
}

// stream reads NDJSON until the server ends, ctx expires or yield returns false.
func stream[T any](ctx context.Context, c *Client, path string) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		resp, err := c.do(ctx, http.MethodGet, path, nil)
		if err != nil {
			yield(zero, err)
			return
		}
		defer resp.Body.Close()
		derr, rerr := ndjson(resp.Body, func(v T) bool { return yield(v, nil) })
		if derr != nil {
			yield(zero, derr)
		} else if rerr != nil && ctx.Err() == nil {
			yield(zero, rerr)
		}
	}
}

// Info asks the daemon for version, driver and state.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var i Info
	return i, c.call(ctx, http.MethodGet, "/info", nil, &i)
}

// List returns the boxes (all = including ended ones).
func (c *Client) List(ctx context.Context, all bool) ([]Box, error) {
	var out []Box
	return out, c.call(ctx, http.MethodGet, "/boxes?all="+strconv.FormatBool(all), nil, &out)
}

// Create creates a box (and starts it with req.Start). req.Attach is ignored; see Attach.
func (c *Client) Create(ctx context.Context, req CreateRequest) (Box, error) {
	req.Attach = false
	var b Box
	return b, c.call(ctx, http.MethodPost, "/boxes", req, &b)
}

// Attach creates a box, starts it and returns logs, events and finally the status. If ctx is canceled,
// the daemon stops the box.
func (c *Client) Attach(ctx context.Context, req CreateRequest) iter.Seq2[AttachMsg, error] {
	req.Attach, req.Start = true, true
	return func(yield func(AttachMsg, error) bool) {
		resp, err := c.do(ctx, http.MethodPost, "/boxes", req)
		if err != nil {
			yield(AttachMsg{}, err)
			return
		}
		defer resp.Body.Close()
		done, stopped := false, false
		derr, rerr := ndjson(resp.Body, func(m AttachMsg) bool {
			if m.Type == "error" && m.Error != nil {
				yield(m, m.Error)
				stopped = true
				return false
			}
			done = done || m.Type == "exit"
			if !yield(m, nil) {
				stopped = true
				return false
			}
			return true
		})
		switch {
		case stopped:
		case derr != nil:
			yield(AttachMsg{}, derr)
		case done:
		case rerr != nil:
			yield(AttachMsg{}, rerr)
		case ctx.Err() == nil:
			yield(AttachMsg{}, io.ErrUnexpectedEOF)
		}
	}
}

// Inspect returns the specification and status of a box (without secret values).
func (c *Client) Inspect(ctx context.Context, name string) (Inspect, error) {
	var i Inspect
	return i, c.call(ctx, http.MethodGet, "/boxes/"+url.PathEscape(name), nil, &i)
}

// Start starts an ended box (rebuilds it if it is not loaded). env supplies values for env: secrets, e.g.
// after a restart of the daemon (nil = none).
func (c *Client) Start(ctx context.Context, name string, env map[string]string) (Box, error) {
	var b Box
	var body any
	if len(env) > 0 {
		body = map[string]any{"env": env}
	}
	return b, c.call(ctx, http.MethodPost, "/boxes/"+url.PathEscape(name)+"/start", body, &b)
}

// EnvRefs names all "env:<NAME>" references in the SDK configuration of a specification.
func EnvRefs(spec tsbox.BoxSpec) []string {
	var out []string
	for _, u := range spec.SDKs {
		for _, v := range u.Config {
			if n, ok := strings.CutPrefix(v, "env:"); ok {
				out = append(out, n)
			}
		}
	}
	return out
}

// LookupEnv collects the values of the env: references from the caller's environment (missing ones are
// left out; the daemon then reports them at start).
func LookupEnv(spec tsbox.BoxSpec) map[string]string {
	out := map[string]string{}
	for _, n := range EnvRefs(spec) {
		if v, ok := os.LookupEnv(n); ok {
			out[n] = v
		}
	}
	return out
}

// Update replaces the specification of a box (see UpdateRequest).
func (c *Client) Update(ctx context.Context, name string, req UpdateRequest) (Box, error) {
	var b Box
	return b, c.call(ctx, http.MethodPut, "/boxes/"+url.PathEscape(name), req, &b)
}

// Emit sends an event to the scripts of a running box and returns to how many it was queued.
func (c *Client) Emit(ctx context.Context, name string, req EmitRequest) (int, error) {
	var r EmitResult
	err := c.call(ctx, http.MethodPost, "/boxes/"+url.PathEscape(name)+"/emit", req, &r)
	return r.Delivered, err
}

// KVNamespaces lists the namespaces of @tsbox/kv (storage area and script) that hold data. A daemon whose
// store cannot list them answers with the code "not_supported"; an older tsboxd without this API with HTTP 404.
func (c *Client) KVNamespaces(ctx context.Context) ([]kvstore.NamespaceInfo, error) {
	var out []kvstore.NamespaceInfo
	return out, c.call(ctx, http.MethodGet, "/kv", nil, &out)
}

// KVEntries returns up to limit entries of one namespace in key order after the key "after" ("" = from the
// start; limit ≤ 0 = 100, at most tsbox.MaxKVPage). Read-only.
func (c *Client) KVEntries(ctx context.Context, storage, script, after string, limit int) (KVPage, error) {
	q := url.Values{}
	if after != "" {
		q.Set("after", after)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/kv/" + url.PathEscape(storage) + "/" + url.PathEscape(script)
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var p KVPage
	return p, c.call(ctx, http.MethodGet, path, nil, &p)
}

// Stop stops a box; grace is the deadline for the scripts (0 = default).
func (c *Client) Stop(ctx context.Context, name string, grace time.Duration) (Box, error) {
	var b Box
	return b, c.call(ctx, http.MethodPost, "/boxes/"+url.PathEscape(name)+"/stop?grace="+grace.String(), nil, &b)
}

// Restart stops and starts a box.
func (c *Client) Restart(ctx context.Context, name string, grace time.Duration) (Box, error) {
	var b Box
	return b, c.call(ctx, http.MethodPost, "/boxes/"+url.PathEscape(name)+"/restart?grace="+grace.String(), nil, &b)
}

// Remove removes a box (force = also a running one).
func (c *Client) Remove(ctx context.Context, name string, force bool) error {
	return c.call(ctx, http.MethodDelete, "/boxes/"+url.PathEscape(name)+"?force="+strconv.FormatBool(force), nil, nil)
}

// LogQuery selects log lines.
type LogQuery struct {
	Follow bool
	Since  time.Time
	Script string
	Tail   int
}

// Logs returns the log lines of a box (with Follow until the box ends or ctx expires).
func (c *Client) Logs(ctx context.Context, name string, q LogQuery) iter.Seq2[tsbox.LogLine, error] {
	v := url.Values{}
	v.Set("follow", strconv.FormatBool(q.Follow))
	if !q.Since.IsZero() {
		v.Set("since", q.Since.Format(time.RFC3339Nano))
	}
	if q.Script != "" {
		v.Set("script", q.Script)
	}
	if q.Tail > 0 {
		v.Set("tail", strconv.Itoa(q.Tail))
	}
	return stream[tsbox.LogLine](ctx, c, "/boxes/"+url.PathEscape(name)+"/logs?"+v.Encode())
}

// Stats returns measurements of running boxes (box = "" for all); with stream, anew every second.
func (c *Client) Stats(ctx context.Context, box string, streamIt bool) iter.Seq2[[]StatsEntry, error] {
	v := url.Values{}
	v.Set("stream", strconv.FormatBool(streamIt))
	if box != "" {
		v.Set("box", box)
	}
	return stream[[]StatsEntry](ctx, c, "/stats?"+v.Encode())
}

// Events returns events since since (ring buffer of the last 1000) and then new ones until ctx expires.
func (c *Client) Events(ctx context.Context, since time.Time, box string) iter.Seq2[tsbox.Event, error] {
	v := url.Values{}
	if !since.IsZero() {
		v.Set("since", since.Format(time.RFC3339Nano))
	}
	if box != "" {
		v.Set("box", box)
	}
	return stream[tsbox.Event](ctx, c, "/events?"+v.Encode())
}

// ReleaseSDK asks the daemon to end the provider processes of an installed SDK (before its files are
// replaced or removed). Error code "in_use" while a running box uses it.
func (c *Client) ReleaseSDK(ctx context.Context, name string) error {
	return c.call(ctx, http.MethodPost, "/sdks/"+url.PathEscape(name)+"/release", nil, nil)
}

// Shutdown ends the daemon (running boxes are stopped).
func (c *Client) Shutdown(ctx context.Context) error {
	return c.call(ctx, http.MethodPost, "/shutdown", nil, nil)
}
