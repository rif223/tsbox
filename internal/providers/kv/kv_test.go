package kv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rif223/tsbox/internal/sdktest"
	"github.com/rif223/tsbox/sdk"
)

func session(storage, script string) *sdktest.Session {
	return &sdktest.Session{
		BoxRef: sdk.BoxRef{Storage: storage}, ScriptRef: sdk.ScriptRef{Name: script},
		C: sdk.Config{"quota": "100", "maxValue": "50"},
	}
}

func call(t *testing.T, h sdk.Handler, m string, p any) (map[string]any, error) {
	t.Helper()
	b, _ := json.Marshal(p)
	r, err := h.Call(context.Background(), m, b)
	if err != nil {
		return nil, err
	}
	b, _ = json.Marshal(r)
	var out map[string]any
	json.Unmarshal(b, &out)
	return out, nil
}

func errCode(err error) string {
	var se *sdk.Error
	if errors.As(err, &se) {
		return se.Code
	}
	return ""
}

func TestKV(t *testing.T) {
	root := t.TempDir()
	p := New(root)
	a, err := p.Open(context.Background(), session("box", "a"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := p.Open(context.Background(), session("box", "b"))
	if _, err := call(t, a, "set", map[string]any{"key": "k", "value": map[string]int{"n": 1}}); err != nil {
		t.Fatal(err)
	}
	if r, _ := call(t, a, "get", map[string]any{"key": "k"}); r["found"] != true {
		t.Fatalf("%v", r)
	}
	if r, _ := call(t, b, "get", map[string]any{"key": "k"}); r["found"] != false {
		t.Fatalf("namespaces not separated: %v", r)
	}
	// Persistence: new provider, same data
	a2, _ := New(root).Open(context.Background(), session("box", "a"))
	if r, _ := call(t, a2, "list", map[string]any{"prefix": ""}); len(r["keys"].([]any)) != 1 {
		t.Fatalf("not persistent: %v", r)
	}
	if _, err := call(t, a, "set", map[string]any{"key": "big", "value": string(make([]byte, 60))}); errCode(err) != sdk.TooLarge {
		t.Fatalf("value limit: %v", err)
	}
	for i := 0; ; i++ {
		_, err := call(t, a, "set", map[string]any{"key": string(rune('a' + i)), "value": "0123456789012345"})
		if err != nil {
			if errCode(err) != sdk.QuotaExceeded {
				t.Fatalf("Quote: %v", err)
			}
			break
		}
		if i > 20 {
			t.Fatal("quota not enforced")
		}
	}
	if r, _ := call(t, a, "delete", map[string]any{"key": "k"}); r["deleted"] != true {
		t.Fatalf("%v", r)
	}
	for _, bad := range []map[string]any{{"key": ""}, {"key": string(make([]byte, 300))}, {"key": "x", "evil": 1}} {
		if _, err := call(t, a, "get", bad); errCode(err) != sdk.InvalidArgument {
			t.Errorf("%v: %v", bad, err)
		}
	}
	if _, err := p.Open(context.Background(), session("../x", "a")); err == nil {
		t.Fatal("path traversal in the storage area accepted")
	}
}

// FuzzKV: broken store files (old format, migrated on first access) and arbitrary parameters for all
// methods – no crash, the new file stays valid, and new values respect the value limit and the quota.
func FuzzKV(f *testing.F) {
	f.Add([]byte(`{"k":1}`), "set", []byte(`{"key":"k","value":"x"}`))
	f.Add([]byte(`{"k":`), "get", []byte(`{"key":"k"}`))
	f.Add([]byte(``), "list", []byte(`{"prefix":"","limit":-1}`))
	f.Add([]byte(`null`), "delete", []byte(`{"key":""}`))
	f.Add([]byte(`{"n":41}`), "incr", []byte(`{"key":"n","by":1,"ttl":1000}`))
	f.Add([]byte(`{}`), "set", []byte(`{"key":"k","value":1,"ifVersion":0}`))
	f.Fuzz(func(t *testing.T, file []byte, method string, params []byte) {
		root := t.TempDir()
		os.MkdirAll(filepath.Join(root, "box"), 0o700)
		os.WriteFile(filepath.Join(root, "box", "a.json"), file, 0o600)
		base := map[string]json.RawMessage{}
		_ = json.Unmarshal(file, &base)
		baseUsed := 0
		for k, v := range base {
			baseUsed += len(k) + len(v)
		}
		h, err := New(root).Open(context.Background(), session("box", "a"))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = h.Call(context.Background(), method, params)
		b, err := os.ReadFile(filepath.Join(root, "box", "v2", "a.json"))
		if err != nil {
			return
		}
		var data struct {
			Entries map[string]struct {
				Value json.RawMessage `json:"value"`
			} `json:"entries"`
		}
		if json.Unmarshal(b, &data) != nil {
			t.Fatalf("store file written broken: %q", b)
		}
		used := 0
		for k, e := range data.Entries {
			used += len(k) + len(e.Value)
			if old, ok := base[k]; (!ok || !bytes.Equal(old, e.Value)) && len(e.Value) > 50 {
				t.Fatalf("value %q above maxValue", k)
			}
		}
		if baseUsed <= 100 && used > 100 {
			t.Fatalf("quota exceeded: %d", used)
		}
	})
}

func TestKVVersionsCounterTTLPaging(t *testing.T) {
	root := t.TempDir()
	h, err := New(root).Open(context.Background(), session("box", "a"))
	if err != nil {
		t.Fatal(err)
	}
	must := func(m string, p any) map[string]any {
		t.Helper()
		r, err := call(t, h, m, p)
		if err != nil {
			t.Fatalf("%s %v: %v", m, p, err)
		}
		return r
	}
	// compare-and-set
	if r := must("set", map[string]any{"key": "c", "value": 1, "ifVersion": 0}); r["ok"] != true {
		t.Fatalf("create-only on an absent key: %v", r)
	}
	if r := must("set", map[string]any{"key": "c", "value": 2, "ifVersion": 0}); r["ok"] != false {
		t.Fatalf("create-only on an existing key: %v", r)
	}
	g := must("get", map[string]any{"key": "c"})
	v := g["version"].(float64)
	if r := must("set", map[string]any{"key": "c", "value": 3, "ifVersion": v + 7}); r["ok"] != false {
		t.Fatalf("wrong version accepted: %v", r)
	}
	r := must("set", map[string]any{"key": "c", "value": 3, "ifVersion": v})
	if r["ok"] != true || r["version"].(float64) <= v {
		t.Fatalf("matching version: %v", r)
	}
	if r := must("delete", map[string]any{"key": "c", "ifVersion": v}); r["deleted"] != false {
		t.Fatalf("delete with an old version: %v", r)
	}
	// counter
	if r := must("incr", map[string]any{"key": "n"}); r["value"] != float64(1) {
		t.Fatalf("incr new: %v", r)
	}
	if r := must("incr", map[string]any{"key": "n", "by": -5}); r["value"] != float64(-4) {
		t.Fatalf("incr by: %v", r)
	}
	if _, err := call(t, h, "incr", map[string]any{"key": "c", "by": 1}); err != nil {
		t.Fatalf("incr on an integer value: %v", err)
	}
	must("set", map[string]any{"key": "s", "value": "x"})
	if _, err := call(t, h, "incr", map[string]any{"key": "s"}); errCode(err) != sdk.InvalidArgument {
		t.Fatalf("incr on a string: %v", err)
	}
	// TTL
	must("set", map[string]any{"key": "t", "value": 1, "ttl": 1})
	time.Sleep(10 * time.Millisecond)
	if r := must("get", map[string]any{"key": "t"}); r["found"] != false {
		t.Fatalf("expired key found: %v", r)
	}
	if _, err := call(t, h, "set", map[string]any{"key": "t", "value": 1, "ttl": 0}); errCode(err) != sdk.InvalidArgument {
		t.Fatalf("ttl 0: %v", err)
	}
	must("set", map[string]any{"key": "t", "value": 1, "ttl": 60000})
	if r := must("get", map[string]any{"key": "t"}); r["expiresAt"] == nil {
		t.Fatalf("expiresAt missing: %v", r)
	}
	// paging
	all := must("list", map[string]any{})["keys"].([]any)
	page := must("list", map[string]any{"limit": 2})
	if len(page["keys"].([]any)) != 2 || page["more"] != true {
		t.Fatalf("page 1: %v", page)
	}
	rest := must("list", map[string]any{"after": page["keys"].([]any)[1]})
	if len(rest["keys"].([]any))+2 != len(all) || rest["more"] != false {
		t.Fatalf("page 2: %v of %v", rest, all)
	}
}

func TestKVMigratesOldFiles(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "box"), 0o700)
	os.WriteFile(filepath.Join(root, "box", "a.json"), []byte(`{"k":{"n":1},"z":"x"}`), 0o600)
	h, _ := New(root).Open(context.Background(), session("box", "a"))
	r, err := call(t, h, "get", map[string]any{"key": "k"})
	if err != nil || r["found"] != true || r["version"] == nil {
		t.Fatalf("%v %v", r, err)
	}
	if _, err := os.Stat(filepath.Join(root, "box", "a.json")); !os.IsNotExist(err) {
		t.Fatalf("old file still there: %v", err)
	}
	h2, _ := New(root).Open(context.Background(), session("box", "a"))
	if r, _ := call(t, h2, "list", map[string]any{}); len(r["keys"].([]any)) != 2 {
		t.Fatalf("after migration: %v", r)
	}
}
