// External provider for the canary test: keeps the secret on the host and hands out only its length.
interface Frame {
  t: string;
  sid?: string;
  id?: number;
  m?: string;
  p?: unknown;
}

const enc = new TextEncoder();
const dec = new TextDecoder();
let token = "";

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
      token = (f.p as { config: Record<string, string> }).config.token ?? "";
      send({ t: "init_ok", p: { name: "secretlen", version: "0.1.0" } });
      return;
    case "open":
      send({ t: "opened", sid: f.sid });
      return;
    case "ping":
      send({ t: "pong", id: f.id } as Frame);
      return;
    case "call":
      send({ t: "ret", sid: f.sid, id: f.id, p: { n: token.length } });
      return;
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
