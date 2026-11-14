package daemon

import (
	"bytes"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rif223/tsbox/client"
)

// FuzzLoad: a damaged state (tsboxd.json) is skipped, never assigned to the wrong box.
func FuzzLoad(f *testing.F) {
	f.Add("bot", []byte(`{"spec":{"name":"bot","scripts":[{"name":"main","entry":"/a.ts"}]},"desired":"running","last":{"state":"running"}}`))
	f.Add("bot", []byte(`{"spec":{"name":"other"}}`))
	f.Add("x", []byte(`{"spec":`))
	f.Fuzz(func(t *testing.T, name string, state []byte) {
		if name == "" || filepath.Base(name) != name || name == "." || name == ".." {
			return
		}
		d := &Daemon{cfg: Config{StateDir: t.TempDir()}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		if err := os.MkdirAll(d.boxDir(name), 0o700); err != nil {
			return
		}
		os.WriteFile(filepath.Join(d.boxDir(name), stateFile), state, 0o600)
		for _, r := range d.load() {
			if r.name != name || r.spec.Name != name {
				t.Fatalf("state of %q loaded as %q", name, r.name)
			}
		}
	})
}

// FuzzDecode: requests to the API are decoded strictly (unknown fields, size) without crashing.
func FuzzDecode(f *testing.F) {
	f.Add([]byte(`{"spec":{"name":"a","scripts":[{"name":"main","entry":"/a.ts"}]},"start":true,"env":{"X":"1"}}`))
	f.Add([]byte(`{"spec":{"name":"a"},"unknown":1}`))
	f.Add([]byte(`{"grace":"5s"}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		var req client.CreateRequest
		_ = decode(httptest.NewRequest("POST", "/v1/boxes", bytes.NewReader(body)), &req)
		var m map[string]any
		_ = decode(httptest.NewRequest("POST", "/v1/boxes/a/stop", bytes.NewReader(body)), &m)
	})
}
