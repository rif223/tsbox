package kvstore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestFileStore(t *testing.T) {
	ctx := context.Background()
	s := NewFileStore(t.TempDir())
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	ns := Namespace{Storage: "box", Script: "a"}
	set := func(key, v string, exp time.Time) (*Entry, error) {
		return s.Update(ctx, ns, key, 40, func(*Entry) (*Entry, error) {
			return &Entry{Value: json.RawMessage(v), Expires: exp}, nil
		})
	}
	e1, err := set("a", `1`, time.Time{})
	if err != nil || e1.Version == 0 {
		t.Fatal(e1, err)
	}
	e2, _ := set("a", `2`, time.Time{})
	if e2.Version <= e1.Version {
		t.Fatalf("version did not grow: %d → %d", e1.Version, e2.Version)
	}
	if _, err := set("big", `"0123456789012345678901234567890123"`, time.Time{}); !errors.Is(err, ErrQuota) {
		t.Fatalf("quota: %v", err)
	}
	// expired entries are absent, free their quota and are not listed
	if _, err := set("t", `"01234567890123456789"`, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get(ctx, ns, "t"); !ok {
		t.Fatal("entry missing before expiry")
	}
	now = now.Add(2 * time.Second)
	if _, ok, _ := s.Get(ctx, ns, "t"); ok {
		t.Fatal("expired entry found")
	}
	if keys, _, _ := s.List(ctx, ns, "", "", 0); len(keys) != 1 {
		t.Fatalf("list: %v", keys)
	}
	if _, err := set("u", `"01234567890123456789"`, time.Time{}); err != nil {
		t.Fatalf("expired entry still counted against the quota: %v", err)
	}
	// delete, then re-create: a new version
	if _, err := s.Update(ctx, ns, "a", 40, func(*Entry) (*Entry, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	e3, _ := set("a", `3`, time.Time{})
	if e3.Version <= e2.Version {
		t.Fatalf("version after re-create: %d ≤ %d", e3.Version, e2.Version)
	}
	// persistence
	s2 := NewFileStore(s.root)
	if e, ok, _ := s2.Get(ctx, ns, "a"); !ok || e.Version != e3.Version || string(e.Value) != "3" {
		t.Fatalf("reloaded: %+v %v", e, ok)
	}
	if _, err := s.Update(ctx, Namespace{Storage: "..", Script: "a"}, "k", 1, nil); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func TestFileStoreConcurrentIncrements(t *testing.T) {
	ctx := context.Background()
	s := NewFileStore(t.TempDir())
	ns := Namespace{Storage: "box", Script: "a"}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				_, err := s.Update(ctx, ns, "n", 1<<20, func(cur *Entry) (*Entry, error) {
					n := 0
					if cur != nil {
						n, _ = strconv.Atoi(string(cur.Value))
					}
					return &Entry{Value: json.RawMessage(strconv.Itoa(n + 1))}, nil
				})
				if err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if e, _, _ := s.Get(ctx, ns, "n"); string(e.Value) != "200" {
		t.Fatalf("lost updates: %s", e.Value)
	}
}

func TestFileStoreNamespaces(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s := NewFileStore(root)
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	set := func(ns Namespace, key, v string, exp time.Time) {
		t.Helper()
		if _, err := s.Update(ctx, ns, key, 1<<20, func(*Entry) (*Entry, error) {
			return &Entry{Value: json.RawMessage(v), Expires: exp}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	set(Namespace{"Shop", "orders"}, "a", `1`, time.Time{})
	set(Namespace{"Shop", "orders"}, "b", `"xy"`, now.Add(time.Hour))
	set(Namespace{"Shop", "orders"}, "gone", `0`, now.Add(time.Second))
	set(Namespace{"only-expired", "main"}, "x", `1`, now.Add(time.Second))
	// tsbox ≤ 0.3, not accessed yet
	if err := os.MkdirAll(filepath.Join(root, "old"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "old", "main.json"), []byte(`{"k":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// not namespaces
	_ = os.WriteFile(filepath.Join(root, "old", "x.json.damaged-1"), []byte(`{`), 0o600)
	_ = os.MkdirAll(filepath.Join(root, ".hidden"), 0o700)
	now = now.Add(2 * time.Second)

	got, err := s.Namespaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Storage != "Shop" || got[1].Storage != "old" {
		t.Fatalf("namespaces %+v", got)
	}
	if g := got[0]; g.Script != "orders" || g.Keys != 2 || g.Bytes != int64(len("a1")+len(`b"xy"`)) || g.Modified.IsZero() {
		t.Fatalf("Shop/orders %+v", g)
	}
	if g := got[1]; g.Script != "main" || g.Keys != 1 {
		t.Fatalf("old/main %+v", g)
	}
	// listing migrated the old file like any access
	if _, err := os.Stat(filepath.Join(root, "old", "main.json")); !os.IsNotExist(err) {
		t.Fatalf("old file still there: %v", err)
	}
	if empty, err := NewFileStore(filepath.Join(root, "missing")).Namespaces(ctx); err != nil || len(empty) != 0 {
		t.Fatalf("missing root: %v %v", empty, err)
	}
}
