// Package log is the built-in provider of @tsbox/log: structured lines in the script's log.
package log

import (
	"context"
	"encoding/json"

	"github.com/rif223/tsbox/sdk"
)

// Provider writes via Session.ScriptLog (the core's rate limit and length limit apply).
type Provider struct{}

func (Provider) Open(_ context.Context, s sdk.Session) (sdk.Handler, error) {
	r := sdk.NewRouter(s)
	sdk.Method(r, "write", func(_ context.Context, s sdk.Session, q struct {
		Level  string         `json:"level"`
		Msg    string         `json:"msg"`
		Fields map[string]any `json:"fields"`
	}) (any, error) {
		switch q.Level {
		case "debug", "info", "warn", "error":
		default:
			return nil, sdk.Errorf(sdk.InvalidArgument, "level: debug, info, warn or error")
		}
		msg := q.Msg
		if len(q.Fields) > 0 {
			b, _ := json.Marshal(q.Fields)
			msg += " " + string(b)
		}
		s.ScriptLog(q.Level, msg)
		return map[string]any{}, nil
	})
	return r, nil
}
