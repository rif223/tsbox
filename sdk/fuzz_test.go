package sdk

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

type fuzzSession struct{}

func (fuzzSession) Box() BoxRef                 { return BoxRef{} }
func (fuzzSession) Script() ScriptRef           { return ScriptRef{} }
func (fuzzSession) Grants() Grants              { return Grants{} }
func (fuzzSession) Config() Config              { return Config{"n": "x", "b": "1Mi", "d": "-5s"} }
func (fuzzSession) Logger() *slog.Logger        { return slog.Default() }
func (fuzzSession) Emit(string, any) error      { return nil }
func (fuzzSession) ScriptLog(level, msg string) {}

// FuzzRouter: parameters from the box are decoded strictly; errors are always *Error with a code.
func FuzzRouter(f *testing.F) {
	f.Add("m", []byte(`{"a":"x","b":[1,2],"c":{"d":true}}`))
	f.Add("m", []byte(`{"a":1}`))
	f.Add("x", []byte(`null`))
	f.Fuzz(func(t *testing.T, method string, params []byte) {
		r := NewRouter(fuzzSession{})
		type req struct {
			A string           `json:"a"`
			B []int            `json:"b"`
			C map[string]bool  `json:"c"`
			D *json.RawMessage `json:"d"`
		}
		Method(r, "m", func(_ context.Context, _ Session, q req) (req, error) { return q, nil })
		if _, err := r.Call(context.Background(), method, params); err != nil {
			if _, ok := err.(*Error); !ok {
				t.Fatalf("error without code: %T %v", err, err)
			}
		}
		c := fuzzSession{}.Config()
		_, _, _, _ = c.Int("n"), c.Bytes("b"), c.Duration("d"), c.Bool(method)
	})
}
