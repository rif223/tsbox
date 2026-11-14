package httpp

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/rif223/tsbox/internal/sdktest"
	"github.com/rif223/tsbox/sdk"
)

// FuzzFetch: arbitrary requests from the box never lead to a connection to a host that was not granted
// (Dial is intercepted, there is no network).
func FuzzFetch(f *testing.F) {
	f.Add(`{"url":"https://api.example.com/x","method":"POST","headers":{"X":"1"},"body":"b"}`)
	f.Add(`{"url":"https://evil.example.com@api.example.com/"}`)
	f.Add(`{"url":"http://127.0.0.1:443/","headers":{"Host":"api.example.com"}}`)
	f.Add(`{"url":"https://API.EXAMPLE.COM./","method":"CONNECT"}`)
	f.Fuzz(func(t *testing.T, params string) {
		var mu sync.Mutex
		var dialed []string
		p := Provider{Dial: func(_ context.Context, _, addr string) (net.Conn, error) {
			mu.Lock()
			defer mu.Unlock()
			dialed = append(dialed, addr)
			return nil, errors.New("no network in the fuzz test")
		}}
		h, err := p.Open(context.Background(), &sdktest.Session{G: sdk.Grants{"host": {"api.example.com"}},
			C: sdk.Config{"timeout": "1s", "allowHTTP": "true"}})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = h.Call(context.Background(), "fetch", []byte(params))
		mu.Lock()
		defer mu.Unlock()
		for _, a := range dialed {
			host, _, _ := net.SplitHostPort(a)
			if !strings.EqualFold(strings.TrimSuffix(host, "."), "api.example.com") {
				t.Fatalf("connection to %q (parameters %s)", a, params)
			}
		}
	})
}

// FuzzAllowed: a host pattern only grants the host itself or its subdomains.
func FuzzAllowed(f *testing.F) {
	f.Add("*.example.com", "https://a.example.com/")
	f.Add("example.com:8443", "https://example.com:8443/")
	f.Add("example.com", "https://example.com.evil.org/")
	f.Fuzz(func(t *testing.T, pattern, raw string) {
		u, err := url.Parse(raw)
		if err != nil || !Allowed([]string{pattern}, u) {
			return
		}
		h, p := strings.ToLower(u.Hostname()), strings.ToLower(strings.TrimPrefix(pattern, "*."))
		p, _, _ = strings.Cut(p, ":")
		if h != p && !strings.HasSuffix(h, "."+p) {
			t.Fatalf("%q grants %q", pattern, u.Host)
		}
	})
}
