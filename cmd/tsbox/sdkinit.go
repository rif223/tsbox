package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var sdkName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// sdkInit creates a working SDK skeleton: manifest, TS module, example.ts, provider (Deno or Go).
func sdkInit(args []string) int {
	fs := flag.NewFlagSet("sdk init", flag.ContinueOnError)
	provider := fs.String("provider", "deno", "provider: deno | go")
	parent := fs.String("dir", ".", "target directory (the SDK ends up in <dir>/<name>)")
	usageFor(fs, "sdk init <name> [--provider deno|go] [--dir .]")
	name, ok := parseArgs(fs, args, true)
	if !ok {
		return 2
	}
	if !sdkName.MatchString(name) {
		return usagef("invalid SDK name %q (a-z, 0-9, -, at most 32 characters)", name)
	}
	dir := filepath.Join(*parent, name)
	if _, err := os.Stat(dir); err == nil {
		return failf("%s already exists", dir)
	}
	files := map[string]string{
		"mod.ts":     tmplMod,
		"example.ts": tmplExample,
		"README.md":  tmplReadme,
	}
	switch *provider {
	case "deno":
		files["tsbox-sdk.json"] = strings.Replace(tmplManifest, "EXEC", `"deno", "run", "--no-prompt", "--no-config", "provider/provider.ts"`, 1)
		files["provider/provider.ts"] = tmplDenoProvider
	case "go":
		files["tsbox-sdk.json"] = strings.Replace(tmplManifest, "EXEC", `"./provider/provider"`, 1)
		files["provider/main.go"] = tmplGoProvider
		files["provider/go.mod"] = "module NAME-provider\n\ngo 1.24\n"
	default:
		return usagef("--provider %q: allowed are deno and go", *provider)
	}
	imp := "@sdk/" + name
	for rel, content := range files {
		content = strings.ReplaceAll(content, "IMPORT", imp)
		content = strings.ReplaceAll(content, "NAME", name)
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return failf("%v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return failf("%v", err)
		}
	}
	fmt.Printf("SDK %q created in %s (import %s).\n\n", name, dir, imp)
	if *provider == "go" {
		bin := "provider"
		if runtime.GOOS == "windows" {
			bin = "provider.exe" // Windows starts only files with an .exe extension
		}
		fmt.Printf("  go build -C %s -o %s .\n", hintPath(filepath.Join(dir, "provider")), bin)
	}
	fmt.Printf("  tsbox sdk test %s --grant %s:greeting=Hello\n  tsbox run %s --sdk %s\n",
		hintPath(dir), name, hintPath(filepath.Join(dir, "example.ts")), hintPath(dir))
	return 0
}

// hintPath writes a path for a command to copy: with / (works in PowerShell, cmd and Git Bash), relative
// paths with "./" (a bare "greet" would be read as an SDK name by --sdk), in quotes if it has spaces.
func hintPath(p string) string {
	p = filepath.ToSlash(p)
	if !filepath.IsAbs(p) && !strings.HasPrefix(p, "./") && !strings.HasPrefix(p, "../") && p != "." {
		p = "./" + p
	}
	if strings.ContainsAny(p, " \t&;|()<>'`$") {
		p = `"` + p + `"`
	}
	return p
}

const tmplManifest = `{
  "name": "NAME",
  "version": "0.1.0",
  "tsbox": "^0.1",
  "description": "Description of NAME",
  "import": "IMPORT",
  "provider": { "exec": [EXEC] },
  "config": {
    "suffix": { "type": "string", "default": "!", "description": "example of an operator setting" }
  },
  "permissions": {
    "greeting": { "type": "string", "description": "greeting the script may use (default: Hello)" }
  },
  "methods": {
    "hello": { "rate": "10/s", "burst": 20, "concurrency": 4, "maxParams": "1Ki", "maxResult": "1Ki", "timeout": "5s" }
  }
}
`

const tmplMod = `/**
 * IMPORT – types and a thin wrapper over @tsbox/core. Every function calls a method from tsbox-sdk.json.
 * @module
 */
import { call } from "@tsbox/core";

/** Greets name with the granted greeting (--grant NAME:greeting=…). */
export async function hello(name: string): Promise<string> {
  return (await call<{ text: string }>("NAME", "hello", { name })).text;
}
`

const tmplExample = `// Example and test: "tsbox sdk test" runs this file in a box and expects exit 0.
import { hello } from "IMPORT";

const text = await hello("World");
console.log(text);
if (!text.includes("World")) throw new Error("unexpected answer: " + text);
`

const tmplReadme = "# NAME\n\nSDK for tsbox.\n\n```bash\ntsbox sdk test .\ntsbox run example.ts --sdk . --grant NAME:greeting=Hello\n```\n\n" +
	"- `tsbox-sdk.json`: manifest (methods with limits, rights, settings)\n" +
	"- `mod.ts`: what the script imports (`import { hello } from \"IMPORT\"`)\n" +
	"- `provider/`: host side, speaks the provider protocol over stdin/stdout\n"

const tmplDenoProvider = `/**
 * Provider for NAME: runs on the host and speaks tsbox's provider protocol over stdin/stdout
 * (frames "u32 big endian length | JSON"). See tsbox/sdks/echo for an example with events.
 */
interface Frame {
  t: string;
  sid?: string;
  id?: number;
  m?: string;
  p?: unknown;
  err?: { code: string; msg?: string };
}

const enc = new TextEncoder();
const dec = new TextDecoder();
const greetings = new Map<string, string>();
let suffix = "!";

function send(f: Frame): void {
  const body = enc.encode(JSON.stringify(f));
  const out = new Uint8Array(4 + body.length);
  new DataView(out.buffer).setUint32(0, body.length);
  out.set(body, 4);
  let off = 0;
  while (off < out.length) off += Deno.stdout.writeSync(out.subarray(off));
}

function handle(f: Frame): void {
  switch (f.t) {
    case "init":
      suffix = (f.p as { config: Record<string, string> }).config.suffix ?? suffix;
      return send({ t: "init_ok" });
    case "open": {
      const grants = (f.p as { grants?: Record<string, string[]> }).grants ?? {};
      greetings.set(f.sid!, grants.greeting?.[0] ?? "Hello");
      return send({ t: "opened", sid: f.sid });
    }
    case "close":
      greetings.delete(f.sid!);
      return;
    case "ping":
      return send({ t: "pong", id: f.id });
    case "call":
      if (f.m === "hello") {
        const { name } = f.p as { name?: unknown };
        if (typeof name !== "string") {
          return send({ t: "ret", sid: f.sid, id: f.id, err: { code: "invalid_argument", msg: "name missing" } });
        }
        return send({ t: "ret", sid: f.sid, id: f.id, p: { text: greetings.get(f.sid!) + ", " + name + suffix } });
      }
      return send({ t: "ret", sid: f.sid, id: f.id, err: { code: "not_found", msg: "method " + f.m + " unknown" } });
  }
}

let buf = new Uint8Array(0);
for await (const chunk of Deno.stdin.readable) {
  const next = new Uint8Array(buf.length + chunk.length);
  next.set(buf);
  next.set(chunk, buf.length);
  buf = next;
  while (buf.length >= 4) {
    const n = new DataView(buf.buffer, buf.byteOffset, 4).getUint32(0);
    if (n === 0 || n > 1 << 20) Deno.exit(2);
    if (buf.length < 4 + n) break;
    handle(JSON.parse(dec.decode(buf.subarray(4, 4 + n))));
    buf = buf.subarray(4 + n);
  }
}
`

const tmplGoProvider = `// Provider for NAME in Go: speaks tsbox's provider protocol over stdin/stdout
// (frames "u32 big endian length | JSON"). Build: go build -o provider . (Windows: -o provider.exe)
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
)

type frame struct {
	T   string          ` + "`json:\"t\"`" + `
	SID string          ` + "`json:\"sid,omitempty\"`" + `
	ID  uint64          ` + "`json:\"id,omitempty\"`" + `
	M   string          ` + "`json:\"m,omitempty\"`" + `
	P   json.RawMessage ` + "`json:\"p,omitempty\"`" + `
	Err *ferr           ` + "`json:\"err,omitempty\"`" + `
}

type ferr struct {
	Code string ` + "`json:\"code\"`" + `
	Msg  string ` + "`json:\"msg,omitempty\"`" + `
}

var out = bufio.NewWriter(os.Stdout)

func send(f frame) {
	b, _ := json.Marshal(f)
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	out.Write(hdr[:])
	out.Write(b)
	out.Flush()
}

func main() {
	in := bufio.NewReader(os.Stdin)
	greetings := map[string]string{}
	suffix := "!"
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(in, hdr[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(hdr[:])
		if n == 0 || n > 1<<20 {
			os.Exit(2)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(in, buf); err != nil {
			return
		}
		var f frame
		if json.Unmarshal(buf, &f) != nil {
			os.Exit(2)
		}
		switch f.T {
		case "init":
			var p struct{ Config map[string]string }
			json.Unmarshal(f.P, &p)
			if s, ok := p.Config["suffix"]; ok {
				suffix = s
			}
			send(frame{T: "init_ok"})
		case "open":
			var p struct{ Grants map[string][]string }
			json.Unmarshal(f.P, &p)
			g := "Hello"
			if v := p.Grants["greeting"]; len(v) > 0 {
				g = v[0]
			}
			greetings[f.SID] = g
			send(frame{T: "opened", SID: f.SID})
		case "close":
			delete(greetings, f.SID)
		case "ping":
			send(frame{T: "pong", ID: f.ID})
		case "call":
			if f.M != "hello" {
				send(frame{T: "ret", SID: f.SID, ID: f.ID, Err: &ferr{Code: "not_found", Msg: "method unknown"}})
				continue
			}
			var q struct{ Name string }
			if json.Unmarshal(f.P, &q) != nil || q.Name == "" {
				send(frame{T: "ret", SID: f.SID, ID: f.ID, Err: &ferr{Code: "invalid_argument", Msg: "name missing"}})
				continue
			}
			res, _ := json.Marshal(map[string]string{"text": greetings[f.SID] + ", " + q.Name + suffix})
			send(frame{T: "ret", SID: f.SID, ID: f.ID, P: res})
		}
	}
}
`
