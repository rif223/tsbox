// Package kv is the built-in provider of @tsbox/kv: a key-value store per storage area (box) and script with
// a byte quota, versions for compare-and-set, counters and TTLs. The data lives in a kvstore.Store
// (default: kvstore.FileStore, one JSON file per namespace).
package kv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rif223/tsbox/kvstore"
	"github.com/rif223/tsbox/sdk"
)

const (
	maxKey   = 256
	maxList  = 1000
	maxTTL   = 10 * 365 * 24 * time.Hour
	maxSafe  = 1<<53 - 1 // largest integer JavaScript represents exactly
	minTTLMs = 1
)

var safeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

// Provider serves @tsbox/kv from a store.
type Provider struct {
	store kvstore.Store
	now   func() time.Time
}

// New creates a provider with a FileStore below root.
func New(root string) *Provider { return NewWithStore(kvstore.NewFileStore(root)) }

// NewWithStore creates a provider on any store.
func NewWithStore(s kvstore.Store) *Provider { return &Provider{store: s, now: time.Now} }

func (p *Provider) Open(_ context.Context, s sdk.Session) (sdk.Handler, error) {
	storage, script := s.Box().Storage, s.Script().Name
	if !safeName.MatchString(storage) || !safeName.MatchString(script) || strings.Contains(storage+script, "..") {
		return nil, fmt.Errorf("kv: invalid storage area %q/%q", storage, script)
	}
	ns := kvstore.Namespace{Storage: storage, Script: script}
	quota, maxValue := s.Config().Bytes("quota"), s.Config().Bytes("maxValue")
	st := p.store

	r := sdk.NewRouter(s)
	sdk.Method(r, "get", func(ctx context.Context, _ sdk.Session, q struct {
		Key string `json:"key"`
	}) (any, error) {
		if err := checkKey(q.Key); err != nil {
			return nil, err
		}
		e, ok, err := st.Get(ctx, ns, q.Key)
		if err != nil {
			return nil, err
		}
		if !ok {
			return map[string]any{"found": false}, nil
		}
		return entryResult(e), nil
	})
	sdk.Method(r, "set", func(ctx context.Context, _ sdk.Session, q struct {
		Key       string          `json:"key"`
		Value     json.RawMessage `json:"value"`
		TTL       *int64          `json:"ttl"`       // milliseconds
		IfVersion *uint64         `json:"ifVersion"` // 0 = only if absent
	}) (any, error) {
		if err := checkKey(q.Key); err != nil {
			return nil, err
		}
		if len(q.Value) == 0 || !json.Valid(q.Value) {
			return nil, sdk.Errorf(sdk.InvalidArgument, "value must be JSON")
		}
		if int64(len(q.Value)) > maxValue {
			return nil, sdk.Errorf(sdk.TooLarge, "value larger than %d bytes", maxValue)
		}
		exp, err := p.expiry(q.TTL)
		if err != nil {
			return nil, err
		}
		ok := true
		e, err := st.Update(ctx, ns, q.Key, quota, func(cur *kvstore.Entry) (*kvstore.Entry, error) {
			if !versionMatches(cur, q.IfVersion) {
				ok = false
				return cur, nil
			}
			return &kvstore.Entry{Value: q.Value, Expires: exp}, nil
		})
		if err != nil {
			return nil, storeErr(err, quota)
		}
		if !ok {
			return map[string]any{"ok": false}, nil
		}
		return map[string]any{"ok": true, "version": e.Version}, nil
	})
	sdk.Method(r, "incr", func(ctx context.Context, _ sdk.Session, q struct {
		Key string `json:"key"`
		By  *int64 `json:"by"`
		TTL *int64 `json:"ttl"` // milliseconds, only when the key is created
	}) (any, error) {
		if err := checkKey(q.Key); err != nil {
			return nil, err
		}
		by := int64(1)
		if q.By != nil {
			by = *q.By
		}
		if by > maxSafe || by < -maxSafe {
			return nil, sdk.Errorf(sdk.InvalidArgument, "by outside ±2^53")
		}
		exp, err := p.expiry(q.TTL)
		if err != nil {
			return nil, err
		}
		var n int64
		e, err := st.Update(ctx, ns, q.Key, quota, func(cur *kvstore.Entry) (*kvstore.Entry, error) {
			next := &kvstore.Entry{Expires: exp}
			if cur != nil {
				v, err := strconv.ParseInt(string(cur.Value), 10, 64)
				if err != nil {
					return nil, sdk.Errorf(sdk.InvalidArgument, "value of %q is not an integer", q.Key)
				}
				n, next.Expires = v, cur.Expires
			}
			if (by > 0 && n > maxSafe-by) || (by < 0 && n < -maxSafe-by) {
				return nil, sdk.Errorf(sdk.InvalidArgument, "counter would leave ±2^53")
			}
			n += by
			next.Value = json.RawMessage(strconv.FormatInt(n, 10))
			return next, nil
		})
		if err != nil {
			return nil, storeErr(err, quota)
		}
		return map[string]any{"value": n, "version": e.Version}, nil
	})
	sdk.Method(r, "delete", func(ctx context.Context, _ sdk.Session, q struct {
		Key       string  `json:"key"`
		IfVersion *uint64 `json:"ifVersion"`
	}) (any, error) {
		if err := checkKey(q.Key); err != nil {
			return nil, err
		}
		deleted := false
		_, err := st.Update(ctx, ns, q.Key, quota, func(cur *kvstore.Entry) (*kvstore.Entry, error) {
			if cur == nil || !versionMatches(cur, q.IfVersion) {
				return cur, nil
			}
			deleted = true
			return nil, nil
		})
		if err != nil {
			return nil, storeErr(err, quota)
		}
		return map[string]any{"deleted": deleted}, nil
	})
	sdk.Method(r, "list", func(ctx context.Context, _ sdk.Session, q struct {
		Prefix string `json:"prefix"`
		After  string `json:"after"`
		Limit  int    `json:"limit"`
	}) (any, error) {
		if len(q.Prefix) > maxKey || len(q.After) > maxKey {
			return nil, sdk.Errorf(sdk.InvalidArgument, "prefix and after: at most %d bytes", maxKey)
		}
		limit := q.Limit
		if limit <= 0 || limit > maxList {
			limit = maxList
		}
		keys, more, err := st.List(ctx, ns, q.Prefix, q.After, limit)
		if err != nil {
			return nil, err
		}
		return map[string]any{"keys": keys, "more": more}, nil
	})
	return r, nil
}

func entryResult(e kvstore.Entry) map[string]any {
	out := map[string]any{"found": true, "value": e.Value, "version": e.Version}
	if !e.Expires.IsZero() {
		out["expiresAt"] = e.Expires.UnixMilli()
	}
	return out
}

// versionMatches: nil = any; 0 = the key must be absent; otherwise the current version.
func versionMatches(cur *kvstore.Entry, want *uint64) bool {
	switch {
	case want == nil:
		return true
	case *want == 0:
		return cur == nil
	default:
		return cur != nil && cur.Version == *want
	}
}

func (p *Provider) expiry(ttl *int64) (time.Time, error) {
	if ttl == nil {
		return time.Time{}, nil
	}
	if *ttl < minTTLMs || *ttl > maxTTL.Milliseconds() {
		return time.Time{}, sdk.Errorf(sdk.InvalidArgument, "ttl: 1 ms to 10 years (milliseconds)")
	}
	return p.now().Add(time.Duration(*ttl) * time.Millisecond), nil
}

func storeErr(err error, quota int64) error {
	if errors.Is(err, kvstore.ErrQuota) {
		return sdk.Errorf(sdk.QuotaExceeded, "quota of %d bytes exhausted", quota)
	}
	return err
}

func checkKey(k string) error {
	if k == "" || len(k) > maxKey || !utf8.ValidString(k) {
		return sdk.Errorf(sdk.InvalidArgument, "key: 1-%d bytes of UTF-8", maxKey)
	}
	return nil
}
