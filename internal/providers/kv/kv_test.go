package kv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

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

// FuzzKV: broken store files and arbitrary parameters for all methods – no crash, quota and value limit
// are respected.
func FuzzKV(f *testing.F) {
	f.Add([]byte(`{"k":1}`), "set", []byte(`{"key":"k","value":"x"}`))
	f.Add([]byte(`{"k":`), "get", []byte(`{"key":"k"}`))
	f.Add([]byte(``), "list", []byte(`{"prefix":"","limit":-1}`))
	f.Add([]byte(`null`), "delete", []byte(`{"key":""}`))
	f.Fuzz(func(t *testing.T, file []byte, method string, params []byte) {
		root := t.TempDir()
		os.MkdirAll(filepath.Join(root, "box"), 0o700)
		os.WriteFile(filepath.Join(root, "box", "a.json"), file, 0o600)
		h, err := New(root).Open(context.Background(), session("box", "a"))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = h.Call(context.Background(), method, params)
		b, err := os.ReadFile(filepath.Join(root, "box", "a.json"))
		if err == nil && !bytes.Equal(b, file) {
			var data map[string]json.RawMessage
			if json.Unmarshal(b, &data) != nil {
				t.Fatalf("store file written broken: %q", b)
			}
			used := 0
			for k, v := range data {
				used += len(k) + len(v)
				if len(v) > 50 {
					t.Fatalf("value %q above maxValue", k)
				}
			}
			if used > 100 {
				t.Fatalf("quota exceeded: %d", used)
			}
		}
	})
}
