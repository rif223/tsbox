// Package kv is the built-in provider of @tsbox/kv: a key-value store per storage area (box) and script,
// persisted as a JSON file (replaced atomically) with a byte quota.
package kv

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/rif223/tsbox/internal/fsutil"
	"github.com/rif223/tsbox/sdk"
)

const (
	maxKey  = 256
	maxList = 1000
)

var safeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

// Provider holds all namespaces under root.
type Provider struct {
	root string
	mu   sync.Mutex
	ns   map[string]*namespace
}

func New(root string) *Provider { return &Provider{root: root, ns: map[string]*namespace{}} }

type namespace struct {
	path   string
	mu     sync.Mutex
	loaded bool
	data   map[string]json.RawMessage
	used   int64
}

func (p *Provider) Open(_ context.Context, s sdk.Session) (sdk.Handler, error) {
	storage, script := s.Box().Storage, s.Script().Name
	if !safeName.MatchString(storage) || !safeName.MatchString(script) || strings.Contains(storage+script, "..") {
		return nil, fmt.Errorf("kv: invalid storage area %q/%q", storage, script)
	}
	path := filepath.Join(p.root, storage, script+".json")
	p.mu.Lock()
	ns, ok := p.ns[path]
	if !ok {
		ns = &namespace{path: path}
		p.ns[path] = ns
	}
	p.mu.Unlock()
	quota, maxValue := s.Config().Bytes("quota"), s.Config().Bytes("maxValue")

	r := sdk.NewRouter(s)
	sdk.Method(r, "get", func(_ context.Context, _ sdk.Session, q struct {
		Key string `json:"key"`
	}) (any, error) {
		if err := checkKey(q.Key); err != nil {
			return nil, err
		}
		ns.mu.Lock()
		defer ns.mu.Unlock()
		if err := ns.load(); err != nil {
			return nil, err
		}
		v, ok := ns.data[q.Key]
		if !ok {
			return map[string]any{"found": false}, nil
		}
		return map[string]any{"found": true, "value": v}, nil
	})
	sdk.Method(r, "set", func(_ context.Context, _ sdk.Session, q struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
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
		ns.mu.Lock()
		defer ns.mu.Unlock()
		if err := ns.load(); err != nil {
			return nil, err
		}
		old, had := ns.data[q.Key]
		used := ns.used + int64(len(q.Value))
		if had {
			used -= int64(len(old))
		} else {
			used += int64(len(q.Key))
		}
		if used > quota {
			return nil, sdk.Errorf(sdk.QuotaExceeded, "quota of %d bytes exhausted", quota)
		}
		ns.data[q.Key] = append(json.RawMessage(nil), q.Value...)
		ns.used = used
		if err := ns.save(); err != nil {
			if had {
				ns.data[q.Key] = old
			} else {
				delete(ns.data, q.Key)
			}
			return nil, err
		}
		return map[string]any{}, nil
	})
	sdk.Method(r, "delete", func(_ context.Context, _ sdk.Session, q struct {
		Key string `json:"key"`
	}) (any, error) {
		if err := checkKey(q.Key); err != nil {
			return nil, err
		}
		ns.mu.Lock()
		defer ns.mu.Unlock()
		if err := ns.load(); err != nil {
			return nil, err
		}
		old, had := ns.data[q.Key]
		if !had {
			return map[string]any{"deleted": false}, nil
		}
		delete(ns.data, q.Key)
		ns.used -= int64(len(q.Key) + len(old))
		return map[string]any{"deleted": true}, ns.save()
	})
	sdk.Method(r, "list", func(_ context.Context, _ sdk.Session, q struct {
		Prefix string `json:"prefix"`
	}) (any, error) {
		ns.mu.Lock()
		defer ns.mu.Unlock()
		if err := ns.load(); err != nil {
			return nil, err
		}
		keys := []string{}
		for k := range ns.data {
			if strings.HasPrefix(k, q.Prefix) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		if len(keys) > maxList {
			keys = keys[:maxList]
		}
		return map[string]any{"keys": keys}, nil
	})
	return r, nil
}

func checkKey(k string) error {
	if k == "" || len(k) > maxKey || !utf8.ValidString(k) {
		return sdk.Errorf(sdk.InvalidArgument, "key: 1-%d bytes of UTF-8", maxKey)
	}
	return nil
}

func (n *namespace) load() error {
	if n.loaded {
		return nil
	}
	n.data = map[string]json.RawMessage{}
	b, err := os.ReadFile(n.path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &n.data); err != nil {
			return fmt.Errorf("kv: %s damaged: %w", n.path, err)
		}
	}
	n.used = 0
	for k, v := range n.data {
		n.used += int64(len(k) + len(v))
	}
	n.loaded = true
	return nil
}

func (n *namespace) save() error {
	if err := os.MkdirAll(filepath.Dir(n.path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(n.data)
	if err != nil {
		return err
	}
	tmp := n.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return fsutil.Rename(tmp, n.path)
}
