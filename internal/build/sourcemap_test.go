package build

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestFixSourceMap(t *testing.T) {
	m := `{"version":3,"sources":["../src/a.ts","file:///x/b.ts"],"sourcesContent":["secret","x"],"mappings":"AAAA"}`
	js := "console.log(1);\n//# sourceMappingURL=data:application/json;base64," + base64.StdEncoding.EncodeToString([]byte(m)) + "\n"
	out := fixSourceMap([]byte(js), func(s string) string { return "file:///box/" + s })
	i := strings.LastIndex(string(out), "base64,")
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out[i+7:])))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	srcs := got["sources"].([]any)
	if srcs[0] != "file:///box/../src/a.ts" || srcs[1] != "file:///box/file:///x/b.ts" || got["sourcesContent"] != nil || got["mappings"] != "AAAA" {
		t.Fatalf("%v", got)
	}
	if !strings.HasPrefix(string(out), "console.log(1);\n//# sourceMappingURL=") {
		t.Fatalf("code changed: %q", out)
	}
	if plain := []byte("console.log(1);\n"); string(fixSourceMap(plain, nil)) != string(plain) {
		t.Fatal("bundle without a source map changed")
	}
}

func TestErrorHidesDenoAdvice(t *testing.T) {
	e := &Error{Output: "TS2322 [ERROR]: x\n\nerror: Type checking failed.\n\n  info: The program failed type-checking, but it still might work correctly.\n  hint: Re-run with --no-check to skip type-checking.\n" +
		"TS2307 [ERROR]: Import \"@x/y\" not a dependency\n  hint: If you want to use a JSR or npm package, try running `deno add jsr:@x/y`\n", Hint: "h"}
	s := e.Error()
	for _, bad := range []string{"--no-check", "might work", "deno add"} {
		if strings.Contains(s, bad) {
			t.Errorf("%q still in %q", bad, s)
		}
	}
	if !strings.Contains(s, "TS2322") || !strings.Contains(s, "TS2307") || !strings.HasSuffix(s, "hint: h") {
		t.Errorf("diagnostics lost: %q", s)
	}
}
