package kvstore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rif223/tsbox/internal/fsutil"
)

// FileStore keeps every namespace in memory and as a JSON file <root>/<storage>/v2/<script>.json, replaced
// atomically after every change. It suits the bundled quota (1 MiB per script); larger data belongs in a
// database-backed Store.
//
// Files of tsbox ≤ 0.3 (<root>/<storage>/<script>.json, a plain key → value object) are migrated on first
// access and then removed.
type FileStore struct {
	root string
	now  func() time.Time

	mu sync.Mutex
	ns map[Namespace]*fileNS
}

// NewFileStore creates a FileStore below root.
func NewFileStore(root string) *FileStore {
	return &FileStore{root: root, now: time.Now, ns: map[Namespace]*fileNS{}}
}

type fileNS struct {
	path, legacy string
	mu           sync.Mutex
	loaded       bool
	seq          uint64
	data         map[string]*Entry
}

// fileFormat is the content of a v2 file.
type fileFormat struct {
	Seq     uint64               `json:"seq"`
	Entries map[string]fileEntry `json:"entries"`
}

type fileEntry struct {
	Value   json.RawMessage `json:"value"`
	Version uint64          `json:"ver"`
	Expires int64           `json:"exp,omitempty"` // Unix milliseconds
}

func (s *FileStore) get(ns Namespace) (*fileNS, error) {
	if ns.Storage == "" || ns.Script == "" || strings.ContainsAny(ns.Storage+ns.Script, `/\`) ||
		strings.Contains(ns.Storage+ns.Script, "..") {
		return nil, fmt.Errorf("kvstore: invalid namespace %q/%q", ns.Storage, ns.Script)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.ns[ns]
	if !ok {
		n = &fileNS{path: filepath.Join(s.root, ns.Storage, "v2", ns.Script+".json"),
			legacy: filepath.Join(s.root, ns.Storage, ns.Script+".json")}
		s.ns[ns] = n
	}
	return n, nil
}

func (s *FileStore) Get(_ context.Context, ns Namespace, key string) (Entry, bool, error) {
	n, err := s.get(ns)
	if err != nil {
		return Entry{}, false, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.load(); err != nil {
		return Entry{}, false, err
	}
	e := n.data[key]
	if e == nil || expired(e, s.now()) {
		return Entry{}, false, nil
	}
	return *e, true, nil
}

func (s *FileStore) Update(_ context.Context, ns Namespace, key string, quota int64, fn UpdateFunc) (*Entry, error) {
	n, err := s.get(ns)
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.load(); err != nil {
		return nil, err
	}
	now := s.now()
	purged := n.purge(now)
	cur := n.data[key]
	var arg *Entry
	if cur != nil {
		c := *cur
		arg = &c
	}
	next, err := fn(arg)
	if err != nil {
		if purged {
			_ = n.save()
		}
		return nil, err
	}
	if next == arg {
		if purged {
			if err := n.save(); err != nil {
				return nil, err
			}
		}
		return cur, nil
	}
	if next != nil {
		used := n.used() - Size(key, cur) + Size(key, next)
		if used > quota {
			if purged {
				_ = n.save()
			}
			return nil, ErrQuota
		}
		stored := &Entry{Value: append(json.RawMessage(nil), next.Value...), Expires: next.Expires}
		n.seq++
		stored.Version = n.seq
		n.data[key] = stored
		if err := n.save(); err != nil {
			n.seq--
			if cur != nil {
				n.data[key] = cur
			} else {
				delete(n.data, key)
			}
			return nil, err
		}
		out := *stored
		return &out, nil
	}
	if cur == nil {
		return nil, nil
	}
	delete(n.data, key)
	n.seq++ // a later re-create gets a new version
	if err := n.save(); err != nil {
		n.data[key] = cur
		return nil, err
	}
	return nil, nil
}

func (s *FileStore) List(_ context.Context, ns Namespace, prefix, after string, limit int) ([]string, bool, error) {
	n, err := s.get(ns)
	if err != nil {
		return nil, false, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.load(); err != nil {
		return nil, false, err
	}
	now := s.now()
	keys := []string{}
	for k, e := range n.data {
		if strings.HasPrefix(k, prefix) && k > after && !expired(e, now) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if limit > 0 && len(keys) > limit {
		return keys[:limit], true, nil
	}
	return keys, false, nil
}

func expired(e *Entry, now time.Time) bool { return !e.Expires.IsZero() && !now.Before(e.Expires) }

// purge removes expired entries; true if something was removed.
func (n *fileNS) purge(now time.Time) bool {
	removed := false
	for k, e := range n.data {
		if expired(e, now) {
			delete(n.data, k)
			removed = true
		}
	}
	return removed
}

func (n *fileNS) used() int64 {
	var u int64
	for k, e := range n.data {
		u += Size(k, e)
	}
	return u
}

func (n *fileNS) load() error {
	if n.loaded {
		return nil
	}
	n.data = map[string]*Entry{}
	b, err := os.ReadFile(n.path)
	switch {
	case err == nil:
		var f fileFormat
		if err := json.Unmarshal(b, &f); err != nil {
			return fmt.Errorf("kvstore: %s damaged: %w", n.path, err)
		}
		n.seq = f.Seq
		for k, e := range f.Entries {
			en := &Entry{Value: e.Value, Version: e.Version}
			if e.Expires != 0 {
				en.Expires = time.UnixMilli(e.Expires)
			}
			n.data[k] = en
			n.seq = max(n.seq, e.Version)
		}
	case os.IsNotExist(err):
		if err := n.migrate(); err != nil {
			return err
		}
	default:
		return err
	}
	n.loaded = true
	return nil
}

// migrate takes over a file of tsbox ≤ 0.3 (key → value), writes it in the v2 format and removes it.
func (n *fileNS) migrate() error {
	b, err := os.ReadFile(n.legacy)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	old := map[string]json.RawMessage{}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &old); err != nil {
			return fmt.Errorf("kvstore: %s damaged: %w", n.legacy, err)
		}
	}
	keys := make([]string, 0, len(old))
	for k := range old {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		n.seq++
		n.data[k] = &Entry{Value: old[k], Version: n.seq}
	}
	if err := n.save(); err != nil {
		n.data, n.seq = map[string]*Entry{}, 0
		return err
	}
	_ = os.Remove(n.legacy)
	return nil
}

func (n *fileNS) save() error {
	if err := os.MkdirAll(filepath.Dir(n.path), 0o700); err != nil {
		return err
	}
	f := fileFormat{Seq: n.seq, Entries: make(map[string]fileEntry, len(n.data))}
	for k, e := range n.data {
		fe := fileEntry{Value: e.Value, Version: e.Version}
		if !e.Expires.IsZero() {
			fe.Expires = e.Expires.UnixMilli()
		}
		f.Entries[k] = fe
	}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	tmp := n.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return fsutil.Rename(tmp, n.path)
}
