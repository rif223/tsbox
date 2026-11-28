package httpp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/rif223/tsbox/internal/sdktest"
	"github.com/rif223/tsbox/sdk"
)

func open(t *testing.T, hosts []string, cfg sdk.Config) sdk.Handler {
	t.Helper()
	base := sdk.Config{"maxBody": "1Ki", "timeout": "5s", "userAgent": "t"}
	for k, v := range cfg {
		base[k] = v
	}
	h, err := Provider{}.Open(context.Background(), &sdktest.Session{G: sdk.Grants{"host": hosts}, C: base})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func fetch(h sdk.Handler, u string, hdr map[string]string) (*response, error) {
	p, _ := json.Marshal(request{URL: u, Headers: hdr})
	r, err := h.Call(context.Background(), "fetch", p)
	if err != nil {
		return nil, err
	}
	resp := r.(response)
	return &resp, nil
}

func code(err error) string {
	var se *sdk.Error
	if errors.As(err, &se) {
		return se.Code
	}
	if err == nil {
		return "ok"
	}
	return "other: " + err.Error()
}

func TestFetch(t *testing.T) {
	var gotCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		switch r.URL.Path {
		case "/big":
			w.Write([]byte(strings.Repeat("x", 4096)))
		case "/away":
			http.Redirect(w, r, "http://not-granted.example/", http.StatusFound)
		case "/self":
			http.Redirect(w, r, "/ok", http.StatusFound)
		default:
			http.SetCookie(w, &http.Cookie{Name: "s", Value: "1"})
			w.Write([]byte("hallo"))
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	hostport := u.Host

	priv := open(t, []string{hostport}, sdk.Config{"allowHTTP": "true", "allowPrivate": "true"})
	r, err := fetch(priv, srv.URL+"/ok", map[string]string{"Cookie": "secret", "X-Test": "1"})
	if err != nil || r.Status != 200 || r.Body != "hallo" {
		t.Fatalf("%+v %v", r, err)
	}
	if gotCookie != "" || r.Headers["set-cookie"] != "" {
		t.Fatalf("cookies not removed: %q %q", gotCookie, r.Headers["set-cookie"])
	}
	if r, err := fetch(priv, srv.URL+"/self", nil); err != nil || r.Body != "hallo" {
		t.Fatalf("redirect to the same host: %+v %v", r, err)
	}
	cases := []struct {
		name string
		h    sdk.Handler
		url  string
		want string
	}{
		{"private without allowPrivate", open(t, []string{hostport}, sdk.Config{"allowHTTP": "true"}), srv.URL + "/ok", sdk.PermissionDenied},
		{"http without allowHTTP", open(t, []string{hostport}, sdk.Config{"allowPrivate": "true"}), srv.URL + "/ok", sdk.PermissionDenied},
		{"host not granted", open(t, []string{"api.example.com"}, sdk.Config{"allowHTTP": "true", "allowPrivate": "true"}), srv.URL + "/ok", sdk.PermissionDenied},
		{"redirect to a foreign host", priv, srv.URL + "/away", sdk.PermissionDenied},
		{"too large", priv, srv.URL + "/big", sdk.TooLarge},
		{"scheme file", priv, "file:///etc/passwd", sdk.InvalidArgument},
		{"credentials in the URL", priv, "http://u:p@" + hostport + "/", sdk.InvalidArgument},
	}
	for _, c := range cases {
		if _, err := fetch(c.h, c.url, nil); code(err) != c.want {
			t.Errorf("%s: %s, expected %s", c.name, code(err), c.want)
		}
	}
}

func TestAllowed(t *testing.T) {
	cases := []struct {
		patterns []string
		url      string
		want     bool
	}{
		{[]string{"api.github.com"}, "https://api.github.com/x", true},
		{[]string{"api.github.com"}, "https://API.GitHub.com/x", true},
		{[]string{"api.github.com"}, "https://api.github.com:8443/x", false},
		{[]string{"api.github.com:8443"}, "https://api.github.com:8443/x", true},
		{[]string{"*.example.org"}, "https://a.example.org/", true},
		{[]string{"*.example.org"}, "https://example.org/", false},
		{[]string{"*.example.org"}, "https://evilexample.org/", false},
		{[]string{"example.org"}, "https://example.org.evil.com/", false},
	}
	for _, c := range cases {
		u, _ := url.Parse(c.url)
		if got := Allowed(c.patterns, u); got != c.want {
			t.Errorf("%v %s: %v", c.patterns, c.url, got)
		}
	}
}

func TestBlocked(t *testing.T) {
	for _, a := range []string{"127.0.0.1", "10.1.2.3", "192.168.0.1", "172.16.0.1", "169.254.169.254", "100.64.0.1",
		"0.0.0.0", "::1", "fe80::1", "fd00::1", "::ffff:127.0.0.1", "224.0.0.1",
		"0.1.2.3", "192.0.0.8", "198.18.0.1", "240.0.0.1", "255.255.255.255", "203.0.113.7",
		"64:ff9b::a00:1", "64:ff9b::7f00:1", "2002:a00:1::1", "2002:7f00:1::", "::7f00:1", "::a00:1",
		"64:ff9b:1::1", "2001:0:4136:e378::1", "2001:db8::1", "fec0::1", "100::1"} {
		if !blocked(netip.MustParseAddr(a)) {
			t.Errorf("%s not blocked", a)
		}
	}
	for _, a := range []string{"1.1.1.1", "140.82.112.3", "2606:4700:4700::1111",
		"64:ff9b::101:101", "2002:101:101::1", "2001:4860:4860::8888"} { // NAT64/6to4 of 1.1.1.1 stay allowed
		if blocked(netip.MustParseAddr(a)) {
			t.Errorf("%s blocked", a)
		}
	}
}
