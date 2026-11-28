package kvstore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setValue(t *testing.T, s *FileStore, ns Namespace, key, v string) *Entry {
	t.Helper()
	e, err := s.Update(context.Background(), ns, key, 4<<20, func(*Entry) (*Entry, error) {
		return &Entry{Value: json.RawMessage(v)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func getValue(t *testing.T, s *FileStore, ns Namespace, key string) (string, uint64, bool) {
	t.Helper()
	e, ok, err := s.Get(context.Background(), ns, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(e.Value), e.Version, ok
}

// Small writes to a full namespace append to the journal instead of rewriting the snapshot; a new store
// (restart) sees the same data and versions.
func TestJournalWritesAndReload(t *testing.T) {
	root := t.TempDir()
	s := NewFileStore(root)
	ns := Namespace{Storage: "box", Script: "a"}
	big := `"` + strings.Repeat("x", 1000) + `"`
	for i := range 1000 { // ~1 MiB, compacted along the way
		setValue(t, s, ns, fmt.Sprintf("k%04d", i), big)
	}
	snap := filepath.Join(root, "box", "v2", "a.json")
	before, err := os.Stat(snap)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	var last *Entry
	for i := range 200 {
		last = setValue(t, s, ns, "small", fmt.Sprint(i))
	}
	t.Logf("200 one-byte sets on a 1 MiB namespace: %v", time.Since(t0))
	after, _ := os.Stat(snap)
	if after.ModTime() != before.ModTime() || after.Size() != before.Size() {
		t.Fatal("small writes rewrote the snapshot")
	}
	journal, _ := os.Stat(filepath.Join(root, "box", "v2", "a.log"))
	if journal == nil || journal.Size() > max(minCompact, before.Size())+64 { // compacted once larger
		t.Fatalf("journal larger than the snapshot: %v", journal)
	}
	s2 := NewFileStore(root)
	if v, ver, ok := getValue(t, s2, ns, "small"); !ok || v != "199" || ver != last.Version {
		t.Fatalf("after reload: %s v%d %v (want 199 v%d)", v, ver, ok, last.Version)
	}
	if v, _, ok := getValue(t, s2, ns, "k0999"); !ok || v != big {
		t.Fatal("entry of the snapshot lost")
	}
	if e := setValue(t, s2, ns, "next", "1"); e.Version <= last.Version {
		t.Fatalf("version after reload did not grow: %d", e.Version)
	}
}

// A crash while appending leaves a line without newline: it is dropped, the lines before stay, and later
// writes are readable again.
func TestJournalTornLine(t *testing.T) {
	root := t.TempDir()
	s := NewFileStore(root)
	ns := Namespace{Storage: "box", Script: "a"}
	setValue(t, s, ns, "a", "1")
	setValue(t, s, ns, "b", "2")
	log := filepath.Join(root, "box", "v2", "a.log")
	f, err := os.OpenFile(log, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"k":"c","v":3,"ver":9`) // torn
	_ = f.Close()
	s2 := NewFileStore(root)
	if v, _, ok := getValue(t, s2, ns, "b"); !ok || v != "2" {
		t.Fatalf("line before the torn one: %q %v", v, ok)
	}
	if _, _, ok := getValue(t, s2, ns, "c"); ok {
		t.Fatal("torn line applied")
	}
	setValue(t, s2, ns, "d", "4")
	if v, _, ok := getValue(t, NewFileStore(root), ns, "d"); !ok || v != "4" {
		t.Fatal("write after a torn line lost")
	}
}

// A damaged snapshot no longer blocks the namespace: it is moved aside, the journal still applies.
func TestDamagedSnapshot(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "box", "v2")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.json"), []byte(`{"seq":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.log"), []byte(`{"k":"j","v":7,"ver":5,"seq":5}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewFileStore(root)
	ns := Namespace{Storage: "box", Script: "a"}
	if v, _, ok := getValue(t, s, ns, "j"); !ok || v != "7" {
		t.Fatalf("journal after a damaged snapshot: %q %v", v, ok)
	}
	if e := setValue(t, s, ns, "n", "1"); e.Version <= 5 {
		t.Fatalf("version %d", e.Version)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "a.json.damaged-*")); len(m) != 1 {
		t.Fatalf("damaged file not kept: %v", m)
	}
}

// A crash between rewriting the snapshot and emptying the journal replays lines the snapshot contains.
func TestJournalReplayIsIdempotent(t *testing.T) {
	root := t.TempDir()
	s := NewFileStore(root)
	ns := Namespace{Storage: "box", Script: "a"}
	setValue(t, s, ns, "a", "1")
	setValue(t, s, ns, "a", "2")
	if _, err := s.Update(context.Background(), ns, "b", 1<<20, func(*Entry) (*Entry, error) {
		return &Entry{Value: json.RawMessage("1")}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(context.Background(), ns, "b", 1<<20, func(*Entry) (*Entry, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(root, "box", "v2", "a.log")
	journal, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := s.get(ns)
	n.mu.Lock()
	if err := n.save(); err != nil { // the snapshot now holds everything, the journal is still there
		t.Fatal(err)
	}
	n.mu.Unlock()
	_ = os.WriteFile(log, journal, 0o600)
	s2 := NewFileStore(root)
	if v, ver, ok := getValue(t, s2, ns, "a"); !ok || v != "2" || ver != 2 {
		t.Fatalf("a: %q v%d %v", v, ver, ok)
	}
	if _, _, ok := getValue(t, s2, ns, "b"); ok {
		t.Fatal("deleted entry came back")
	}
}
