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
		return response{Status: resp.StatusCode, URL: resp.Request.URL.String(), Headers: h, Body: string(b)}, nil
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

func blocked(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() ||
		netip.MustParsePrefix("100.64.0.0/10").Contains(a) || // CGNAT
		netip.MustParsePrefix("169.254.0.0/16").Contains(a) || // cloud metadata
		netip.MustParsePrefix("fc00::/7").Contains(a)
}

func forbiddenHeader(k string) bool {
	switch strings.ToLower(k) {
	case "host", "connection", "keep-alive", "proxy-authorization", "proxy-connection", "te", "trailer",
		"transfer-encoding", "upgrade", "content-length", "cookie", "user-agent":
		return true
	}
	return strings.HasPrefix(strings.ToLower(k), "proxy-")
}
