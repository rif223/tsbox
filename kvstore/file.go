package kvstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rif223/tsbox/internal/fsutil"
)

// FileStore keeps every namespace in memory and on disk below <root>/<storage>/v2/: a snapshot
// <script>.json and a journal <script>.log with one JSON line per change, synced before the change counts.
// When the journal outgrows the snapshot (and at least minCompact), the snapshot is rewritten (atomically,
// synced) and the journal starts empty; a write costs one line instead of the whole namespace. Loading
// replays the journal over the snapshot; a line cut off by a crash ends the replay. A damaged snapshot is
// moved aside (<script>.json.damaged-<unix time>) instead of blocking the namespace for good. It suits the
// bundled quota (1 MiB per script); larger data belongs in a database-backed Store.
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
	journalSize  int64 // bytes in the journal
	snapSize     int64 // bytes of the last snapshot
}

// minCompact is the journal size below which the snapshot is not rewritten.
const minCompact = 64 << 10

// fileFormat is the content of a v2 snapshot.
type fileFormat struct {
	Seq     uint64               `json:"seq"`
	Entries map[string]fileEntry `json:"entries"`
}

type fileEntry struct {
	Value   json.RawMessage `json:"value"`
	Version uint64          `json:"ver"`
	Expires int64           `json:"exp,omitempty"` // Unix milliseconds
}

// journalRec is a line of the journal: a set (V, Ver, Exp) or a delete (Del); Seq is the counter afterwards.
type journalRec struct {
	K   string          `json:"k"`
	V   json.RawMessage `json:"v,omitempty"`
	Ver uint64          `json:"ver,omitempty"`
	Exp int64           `json:"exp,omitempty"`
	Del bool            `json:"del,omitempty"`
	Seq uint64          `json:"seq"`
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
	n.purge(s.now()) // in memory only: expired entries count as absent when loading as well
	cur := n.data[key]
	var arg *Entry
	if cur != nil {
		c := *cur
		arg = &c
	}
	next, err := fn(arg)
	if err != nil {
		return nil, err
	}
	if next == arg {
		return cur, nil
	}
	if next != nil {
		used := n.used() - Size(key, cur) + Size(key, next)
		if used > quota {
			return nil, ErrQuota
		}
		stored := &Entry{Value: append(json.RawMessage(nil), next.Value...), Version: n.seq + 1, Expires: next.Expires}
		rec := journalRec{K: key, V: stored.Value, Ver: stored.Version, Seq: stored.Version}
		if !stored.Expires.IsZero() {
			rec.Exp = stored.Expires.UnixMilli()
		}
		if err := n.appendJournal(rec); err != nil {
			return nil, err
		}
		n.seq = stored.Version
		n.data[key] = stored
		n.maybeCompact()
		out := *stored
		return &out, nil
	}
	if cur == nil {
		return nil, nil
	}
	if err := n.appendJournal(journalRec{K: key, Del: true, Seq: n.seq + 1}); err != nil {
		return nil, err
	}
	delete(n.data, key)
	n.seq++ // a later re-create gets a new version
	n.maybeCompact()
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
		if jerr := json.Unmarshal(b, &f); jerr != nil {
			// Before, the namespace stayed unusable until someone deleted the file. Keep it for inspection and go
			// on with the journal (the writes since the last snapshot).
			aside := fmt.Sprintf("%s.damaged-%d", n.path, time.Now().Unix())
			if err := fsutil.Rename(n.path, aside); err != nil {
				return fmt.Errorf("kvstore: %s damaged (%v) and cannot be moved aside: %w", n.path, jerr, err)
			}
			slog.Warn("kvstore: damaged snapshot moved aside", "file", n.path, "to", aside, "err", jerr)
			n.data = map[string]*Entry{}
			break
		}
		n.snapSize = int64(len(b))
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
	if err := n.replay(); err != nil {
		return err
	}
	n.loaded = true
	return nil
}

func (n *fileNS) journalPath() string { return strings.TrimSuffix(n.path, ".json") + ".log" }

// replay applies the journal. Replaying lines that the snapshot already contains (crash between rewriting
// the snapshot and emptying the journal) gives the same state. A line without its newline or not readable
// (crash while writing) is cut off together with everything after it.
func (n *fileNS) replay() error {
	b, err := os.ReadFile(n.journalPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	good := 0
	for good < len(b) {
		i := bytes.IndexByte(b[good:], '\n')
		if i < 0 {
			break
		}
		var r journalRec
		if json.Unmarshal(b[good:good+i], &r) != nil || (!r.Del && len(r.V) == 0) {
			break
		}
		if r.Del {
			delete(n.data, r.K)
		} else {
			e := &Entry{Value: r.V, Version: r.Ver}
			if r.Exp != 0 {
				e.Expires = time.UnixMilli(r.Exp)
			}
			n.data[r.K] = e
		}
		n.seq = max(n.seq, r.Seq, r.Ver)
		good += i + 1
	}
	if good < len(b) {
		slog.Warn("kvstore: incomplete journal line dropped", "file", n.journalPath(), "bytes", len(b)-good)
		if err := os.Truncate(n.journalPath(), int64(good)); err != nil {
			return err
		}
	}
	n.journalSize = int64(good)
	return nil
}

// appendJournal writes one line and syncs it.
func (n *fileNS) appendJournal(r journalRec) error {
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(n.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(n.journalPath(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		// A partial line would end every later replay before the lines after it: cut it off now.
		_ = os.Truncate(n.journalPath(), n.journalSize)
		return err
	}
	n.journalSize += int64(len(line)) + 1
	return nil
}

// maybeCompact rewrites the snapshot once the journal is larger than it (and at least minCompact). A failure
// only leaves the journal longer; the data is safe in it.
func (n *fileNS) maybeCompact() {
	if n.journalSize < minCompact || n.journalSize < n.snapSize {
		return
	}
	if err := n.save(); err != nil {
		slog.Warn("kvstore: rewriting the snapshot failed", "file", n.path, "err", err)
		return
	}
	if err := os.Remove(n.journalPath()); err != nil && !os.IsNotExist(err) {
		slog.Warn("kvstore: emptying the journal failed", "file", n.journalPath(), "err", err)
		return
	}
	n.journalSize = 0
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

// save writes the snapshot atomically: temporary file, sync, rename, sync of the directory.
func (n *fileNS) save() error {
	dir := filepath.Dir(n.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
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
	if err := writeSynced(tmp, b); err != nil {
		return err
	}
	if err := fsutil.Rename(tmp, n.path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil { // makes the rename durable on Linux; a no-op error on Windows
		_ = d.Sync()
		_ = d.Close()
	}
	n.snapSize = int64(len(b))
	return nil
}

func writeSynced(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Namespaces lists the namespaces below root in both layouts (tsbox ≤ 0.3 and v2). Listing loads each one,
// which migrates files of tsbox ≤ 0.3 as any other access does.
func (s *FileStore) Namespaces(ctx context.Context) ([]NamespaceInfo, error) {
	storages, err := os.ReadDir(s.root)
	if os.IsNotExist(err) {
		return []NamespaceInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	var found []Namespace
	for _, st := range storages {
		if !st.IsDir() || !validName(st.Name()) {
			continue
		}
		seen := map[string]bool{}
		add := func(dir string, suffixes ...string) {
			files, _ := os.ReadDir(dir)
			for _, f := range files {
				for _, suf := range suffixes {
					if name, ok := strings.CutSuffix(f.Name(), suf); ok && !f.IsDir() && validName(name) && !seen[name] {
						seen[name] = true
						found = append(found, Namespace{Storage: st.Name(), Script: name})
					}
				}
			}
		}
		add(filepath.Join(s.root, st.Name(), "v2"), ".json", ".log")
		add(filepath.Join(s.root, st.Name()), ".json")
	}
	now := s.now()
	out := make([]NamespaceInfo, 0, len(found))
	for _, ns := range found {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := s.get(ns)
		if err != nil {
			continue
		}
		n.mu.Lock()
		err = n.load()
		info := NamespaceInfo{Namespace: ns}
		if err == nil {
			for k, e := range n.data {
				if !expired(e, now) {
					info.Keys++
					info.Bytes += Size(k, e)
				}
			}
			for _, p := range []string{n.path, n.journalPath(), n.legacy} {
				if fi, err := os.Stat(p); err == nil && fi.ModTime().After(info.Modified) {
					info.Modified = fi.ModTime()
				}
			}
		}
		n.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if info.Keys > 0 {
			out = append(out, info)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Storage != out[j].Storage {
			return out[i].Storage < out[j].Storage
		}
		return out[i].Script < out[j].Script
	})
	return out, nil
}

// validName is the rule for storage areas and script names ([a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}).
func validName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || i > 0 && (r == '_' || r == '.' || r == '-')
		if !ok {
			return false
		}
	}
	return true
}
