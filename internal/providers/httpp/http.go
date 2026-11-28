// Package httpp is the built-in provider of @tsbox/http: fetch only to granted hosts.
//
// Protection against SSRF:
//   - https only (http only with the operator setting allowHTTP)
//   - the host must match a grant ("api.github.com", "*.example.org", optionally ":port")
//   - the IP actually connected to is checked while connecting (Dialer.Control): no private, loopback,
//     link-local, multicast or unspecified addresses (allowPrivate turns this off). So DNS rebinding does
//     not help.
//   - redirects (at most 5) are checked like the first request
//   - no proxy from the environment, no cookies, hop-by-hop and Host headers are dropped
//   - response at most maxBody, time limit timeout
package httpp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/rif223/tsbox/manifest"
	"github.com/rif223/tsbox/sdk"
)

const maxRedirects = 5

// Provider is stateless; every script gets its own client.
type Provider struct {
	// Dial replaces the connection setup (tests only).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

var errBlockedIP = errors.New("target address not allowed (private, loopback or link-local)")

type request struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

type response struct {
	Status  int               `json:"status"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

func (p Provider) Open(_ context.Context, s sdk.Session) (sdk.Handler, error) {
	cfg := s.Config()
	allowHTTP, allowPrivate := cfg.Bool("allowHTTP"), cfg.Bool("allowPrivate")
	maxBody, timeout, ua := cfg.Bytes("maxBody"), cfg.Duration("timeout"), cfg.String("userAgent")
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if maxBody <= 0 {
		maxBody = 1 << 20
	}
	hosts := s.Grants().Values("host")

	dialer := &net.Dialer{
		Timeout: timeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			if allowPrivate {
				return nil
			}
			ap, err := netip.ParseAddrPort(address)
			if err != nil || blocked(ap.Addr()) {
				return errBlockedIP
			}
			return nil
		},
	}
	dial := dialer.DialContext
	if p.Dial != nil {
		dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if !allowPrivate {
				if ap, err := netip.ParseAddrPort(addr); err == nil && blocked(ap.Addr()) {
					return nil, errBlockedIP
				}
			}
			return p.Dial(ctx, network, addr)
		}
	}
	check := func(u *url.URL) error {
		switch u.Scheme {
		case "https":
		case "http":
			if !allowHTTP {
				return sdk.Errorf(sdk.PermissionDenied, "only https allowed")
			}
		default:
			return sdk.Errorf(sdk.InvalidArgument, "scheme %q not allowed", u.Scheme)
		}
		if u.User != nil {
			return sdk.Errorf(sdk.InvalidArgument, "credentials in the URL are not allowed")
		}
		if !Allowed(hosts, u) {
			return sdk.Errorf(sdk.PermissionDenied, "host %q is not granted (--grant http:host=%s)", u.Host, u.Hostname())
		}
		return nil
	}
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: nil, DialContext: dial, ForceAttemptHTTP2: true,
			TLSHandshakeTimeout: timeout, ResponseHeaderTimeout: timeout, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second,
		},
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return sdk.Errorf(sdk.InvalidArgument, "too many redirects")
			}
			return check(req.URL)
		},
	}

	r := sdk.NewRouter(s)
	sdk.Method(r, "fetch", func(ctx context.Context, _ sdk.Session, q request) (any, error) {
		u, err := url.Parse(q.URL)
		if err != nil || u.Host == "" {
			return nil, sdk.Errorf(sdk.InvalidArgument, "invalid URL")
		}
		if err := check(u); err != nil {
			return nil, err
		}
		method := strings.ToUpper(q.Method)
		switch method {
		case "":
			method = "GET"
		case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE":
		default:
			return nil, sdk.Errorf(sdk.InvalidArgument, "method %q not allowed", q.Method)
		}
		var body io.Reader
		if q.Body != "" {
			body = strings.NewReader(q.Body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
		if err != nil {
			return nil, sdk.Errorf(sdk.InvalidArgument, "%v", err)
		}
		for k, v := range q.Headers {
			if forbiddenHeader(k) {
				continue
			}
			req.Header.Set(k, v)
		}
		req.Header.Set("User-Agent", ua)
		resp, err := client.Do(req)
		if err != nil {
			var se *sdk.Error
			switch {
			case errors.As(err, &se):
				return nil, se
			case errors.Is(err, errBlockedIP):
				return nil, sdk.Errorf(sdk.PermissionDenied, "%v", errBlockedIP)
			case errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "Timeout"):
				return nil, sdk.Errorf(sdk.Timeout, "timeout")
			}
			return nil, sdk.Errorf(sdk.Unavailable, "connection failed")
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		if err != nil {
			return nil, sdk.Errorf(sdk.Unavailable, "response aborted")
		}
		if int64(len(b)) > maxBody {
			return nil, sdk.Errorf(sdk.TooLarge, "response larger than %d bytes", maxBody)
		}
		if !utf8.Valid(b) {
			return nil, sdk.Errorf(sdk.InvalidArgument, "response is not UTF-8 text")
		}
		h := map[string]string{}
		for k, v := range resp.Header {
			if k != "Set-Cookie" {
				h[strings.ToLower(k)] = strings.Join(v, ", ")
			}
		}
		out := response{Status: resp.StatusCode, URL: resp.Request.URL.String(), Headers: h, Body: string(b)}
		// The result travels as one frame: the body JSON-escaped (a '"' or a newline takes two bytes) plus the
		// headers must fit an SDK result. Without this check the engine refused it as "result larger than
		// 1020Ki" although the body was below maxBody, and a larger maxBody could not help.
		if enc, err := json.Marshal(out); err == nil && int64(len(enc)) > manifest.MaxResultBytes {
			return nil, sdk.Errorf(sdk.TooLarge, "response of %d bytes is %d bytes as a result (JSON-escaped body and headers), "+
				"more than an SDK result can carry (%s)", len(b), len(enc), manifest.FormatBytes(manifest.MaxResultBytes))
		}
		return out, nil
	})
	r.OnClose(func() error { client.CloseIdleConnections(); return nil })
	return r, nil
}

// Allowed checks the host of a URL against the grants. Without a port in the grant, only the default port
// of the scheme applies.
func Allowed(patterns []string, u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[u.Scheme]
	}
	for _, p := range patterns {
		ph, pp, hasPort := strings.Cut(strings.ToLower(p), ":")
		want := map[string]string{"https": "443", "http": "80"}[u.Scheme]
		if hasPort {
			want = pp
		}
		if port != want {
			continue
		}
		if suffix, ok := strings.CutPrefix(ph, "*."); ok {
			if strings.HasSuffix(host, "."+suffix) {
				return true
			}
		} else if host == ph {
			return true
		}
	}
	return false
}

// blockedPrefixes are the special-purpose ranges (IANA registries) that are not globally reachable unicast,
// besides what netip classifies (loopback, private, link-local, multicast, unspecified).
var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{
		"0.0.0.0/8",       // "this network"
		"100.64.0.0/10",   // CGNAT
		"169.254.0.0/16",  // link-local, cloud metadata
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation
		"192.88.99.0/24",  // 6to4 relay anycast
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation
		"203.0.113.0/24",  // documentation
		"240.0.0.0/4",     // reserved, includes 255.255.255.255
		"::/96",           // IPv4-compatible (deprecated); the embedded IPv4 is checked as well
		"64:ff9b:1::/48",  // local-use NAT64: maps to addresses of the local network
		"100::/64",        // discard
		"2001::/32",       // Teredo: tunnels to an address that is not visible here
		"2001:2::/48",     // benchmarking
		"2001:10::/28",    // ORCHID
		"2001:20::/28",    // ORCHIDv2
		"2001:db8::/32",   // documentation
		"fc00::/7",        // unique local
		"fec0::/10",       // site-local (deprecated)
	} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

var (
	nat64     = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour = netip.MustParsePrefix("2002::/16")
)

// blocked reports whether a may not be connected to: everything but globally reachable unicast. IPv6
// addresses that carry an IPv4 address (NAT64, 6to4, IPv4-compatible) are blocked if that IPv4 address is.
func blocked(a netip.Addr) bool {
	a = a.Unmap()
	if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	if v4, ok := embeddedIPv4(a); ok {
		return blocked(v4)
	}
	return false
}

// embeddedIPv4 returns the IPv4 address that NAT64 (64:ff9b::/96) or 6to4 (2002::/16) addresses carry.
func embeddedIPv4(a netip.Addr) (netip.Addr, bool) {
	if !a.Is6() {
		return netip.Addr{}, false
	}
	b := a.As16()
	switch {
	case nat64.Contains(a):
		return netip.AddrFrom4([4]byte(b[12:16])), true
	case sixToFour.Contains(a):
		return netip.AddrFrom4([4]byte(b[2:6])), true
	}
	return netip.Addr{}, false
}

func forbiddenHeader(k string) bool {
	switch strings.ToLower(k) {
	case "host", "connection", "keep-alive", "proxy-authorization", "proxy-connection", "te", "trailer",
		"transfer-encoding", "upgrade", "content-length", "cookie", "user-agent":
		return true
	}
	return strings.HasPrefix(strings.ToLower(k), "proxy-")
}
