/**
 * External provider for @tsbox/echo. Speaks tsbox's provider protocol over stdin/stdout:
 * frames "u32 big endian length | JSON" (≤ 1 MiB).
 *
 *   tsbox → provider: init{p:{proto,sdk,config}}, open{sid,p:{box,script,grants}}, call{sid,id,m,p}, close{sid}, ping{id}
 *   provider → tsbox: init_ok, opened{sid} (or with err), ret{sid,id,p|err}, emit{sid,name,p}, log{level,msg}, pong{id}
 *
 * Runs on the host (outside the box) as trusted operator code. Meant as a template for your own providers
 * in any language.
 */
interface Frame {
  t: string;
  sid?: string;
  id?: number;
  m?: string;
  name?: string;
  level?: string;
  msg?: string;
  p?: unknown;
  err?: { code: string; msg?: string };
}

interface Session {
  maxTicks: number;
}

const enc = new TextEncoder();
const dec = new TextDecoder();
const sessions = new Map<string, Session>();
let prefix = "echo: ";

function send(f: Frame): void {
  const body = enc.encode(JSON.stringify(f));
  const out = new Uint8Array(4 + body.length);
  new DataView(out.buffer).setUint32(0, body.length);
  out.set(body, 4);
  let off = 0;
  while (off < out.length) off += Deno.stdout.writeSync(out.subarray(off));
}

function fail(sid: string | undefined, id: number | undefined, code: string, msg: string): void {
  send({ t: "ret", sid, id, err: { code, msg } });
}

function handle(f: Frame): void {
  switch (f.t) {
    case "init": {
      const p = f.p as { config: Record<string, string> };
      prefix = p.config.prefix ?? prefix;
      send({ t: "init_ok", p: { name: "echo", version: "0.1.0" } });
      return;
    }
    case "open": {
      const p = f.p as { grants?: Record<string, string[]> };
      const maxTicks = Number(p.grants?.ticks?.[0] ?? "3");
      sessions.set(f.sid!, { maxTicks });
      send({ t: "opened", sid: f.sid });
      return;
    }
    case "close":
      sessions.delete(f.sid!);
      return;
    case "ping":
      send({ t: "pong", id: f.id });
      return;
    case "call": {
      const s = sessions.get(f.sid!);
      if (!s) return fail(f.sid, f.id, "unavailable", "unknown session");
      if (f.m === "echo") {
        const { text } = f.p as { text: string };
        if (typeof text !== "string") return fail(f.sid, f.id, "invalid_argument", "text missing");
        send({ t: "ret", sid: f.sid, id: f.id, p: { text: prefix + text } });
      } else if (f.m === "tick") {
        const n = Math.min(Number((f.p as { n: number }).n) || 0, s.maxTicks);
        for (let i = 1; i <= n; i++) send({ t: "emit", sid: f.sid, name: "tick", p: { n: i } });
        send({ t: "ret", sid: f.sid, id: f.id, p: { sent: n } });
      } else {
        fail(f.sid, f.id, "not_found", `method ${f.m} unknown`);
      }
      return;
    }
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
