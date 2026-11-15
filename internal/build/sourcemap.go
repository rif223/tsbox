package build

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
)

// sourceMapPrefix starts the inline source map that "deno bundle --sourcemap=inline" appends.
var sourceMapPrefix = []byte("//# sourceMappingURL=data:application/json;base64,")

// fixSourceMap rewrites the inline source map of a bundle: every source becomes what resolve returns (an
// absolute URL, so that Deno shows the real file in stack traces instead of a path relative to the
// artifact store), and the embedded sources are dropped (Deno needs only the mappings; the bundle stays
// small). Without a source map b is returned unchanged.
func fixSourceMap(b []byte, resolve func(source string) string) []byte {
	i := bytes.LastIndex(b, sourceMapPrefix)
	if i < 0 {
		return b
	}
	enc := bytes.TrimSpace(b[i+len(sourceMapPrefix):])
	raw, err := base64.StdEncoding.DecodeString(string(enc))
	if err != nil {
		return b
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return b
	}
	if srcs, ok := m["sources"].([]any); ok {
		for k, s := range srcs {
			if str, ok := s.(string); ok {
				srcs[k] = resolve(str)
			}
		}
	}
	delete(m, "sourcesContent")
	out, err := json.Marshal(m)
	if err != nil {
		return b
	}
	res := append([]byte(nil), b[:i]...)
	res = append(res, sourceMapPrefix...)
	res = append(res, base64.StdEncoding.EncodeToString(out)...)
	return append(res, '\n')
}
