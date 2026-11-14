/**
 * Script side of tsbox: stdin/stdout to tsbox-init. stdout belongs exclusively to the protocol – console.*
 * and Deno.stdout are therefore redirected to log frames.
 *
 * Lifetime: stdin is only read "referenced" while something expects an answer (open calls, event
 * subscriptions, keepAlive). Otherwise the process ends like a normal Deno script as soon as its event
 * loop is empty (spike g, docs/spike-2026-11.md).
 * @module
 */
import { encodeFrame, type Frame, FrameDecoder, writeAllSync } from "./frames.ts";

const CALL_TIMEOUT_MS = 60_000;
const ALIVE_INTERVAL_MS = 1_000;
const STATS_INTERVAL_MS = 10_000;
const MAX_LOG = 4096;

/** Error of an SDK call. `code` is machine-readable (e.g. not_found, permission_denied). */
export class SdkError extends Error {
  override name = "SdkError";
  constructor(readonly code: string, message?: string) {
    super(message ? `${code}: ${message}` : code);
  }
}

interface DenoCore {
  read(rid: number, buf: Uint8Array): Promise<number>;
  refOpPromise(p: Promise<unknown>): void;
  unrefOpPromise(p: Promise<unknown>): void;
}

function denoCore(): DenoCore | undefined {
  // deno-lint-ignore no-explicit-any
  const d = Deno as any;
  const c = d[d.internal]?.core;
  return c && typeof c.read === "function" && typeof c.unrefOpPromise === "function" ? c : undefined;
}

type Pending = { resolve: (p: unknown) => void; reject: (e: Error) => void; timer: ReturnType<typeof setTimeout> };
type Handler = (payload: unknown) => void;

// Capture before any script code (boot.ts is evaluated as the first module).
const rawWrite: (p: Uint8Array) => number = Deno.stdout.writeSync.bind(Deno.stdout);

export class Transport {
  #started = false;
  #nextId = 1;
  #pending = new Map<number, Pending>();
  #handlers = new Map<string, Set<Handler>>();
  #keep = 0;
  #core = denoCore();
  #op: Promise<number> | undefined;
  #refed = false;
  #dec = new FrameDecoder();
  #stdoutLine = "";

  /** Name and parameters of the script (passed by tsbox-init in argv, not secret). */
  readonly script: { readonly name: string; readonly params: unknown };

  constructor() {
    let params: unknown = undefined;
    try {
      params = Deno.args[1] ? JSON.parse(Deno.args[1]) : undefined;
    } catch { /* invalid parameters: undefined */ }
    this.script = Object.freeze({ name: Deno.args[0] ?? "", params });
  }

  start(): void {
    if (this.#started) return;
    this.#started = true;
    this.#redirectConsole();
    this.#redirectStdout();
    globalThis.addEventListener("unhandledrejection", (e) => {
      e.preventDefault();
      Deno.exitCode = 1;
      this.log("error", "Unhandled promise rejection: " + formatError(e.reason));
    });
    globalThis.addEventListener("error", (e) => {
      e.preventDefault();
      Deno.exitCode = 1;
      this.log("error", "Unhandled error: " + formatError(e.error ?? e.message));
    });
    // Heartbeat: if it stops, the event loop is blocked (watchdog in tsbox-init).
    Deno.unrefTimer(setInterval(() => this.send({ t: "alive" }), ALIVE_INTERVAL_MS));
    Deno.unrefTimer(setInterval(() => {
      const m = Deno.memoryUsage();
      this.send({ t: "stats", p: { heapUsed: m.heapUsed, heapTotal: m.heapTotal, external: m.external } });
    }, STATS_INTERVAL_MS));
    this.send({ t: "alive" });
    this.#readLoop();
  }

  send(f: Frame): void {
    writeAllSync(rawWrite, encodeFrame(f));
  }

  log(level: "debug" | "info" | "warn" | "error", msg: string): void {
    this.send({ t: "log", level, msg: msg.length > MAX_LOG ? msg.slice(0, MAX_LOG) : msg });
  }

  ready(): void {
    this.send({ t: "ready" });
  }

  call(sdk: string, m: string, p?: unknown, timeoutMs = CALL_TIMEOUT_MS): Promise<unknown> {
    const id = this.#nextId++;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.#pending.delete(id);
        this.#sync();
        reject(new SdkError("timeout", `${sdk}.${m} after ${timeoutMs} ms`));
      }, timeoutMs);
      Deno.unrefTimer(timer);
      this.#pending.set(id, { resolve, reject, timer });
      this.#sync();
      this.send({ t: "call", id, sdk, m, p });
    });
  }

  on(sdk: string, event: string, fn: Handler): () => void {
    const key = `${sdk}:${event}`;
    let set = this.#handlers.get(key);
    if (!set) {
      set = new Set();
      this.#handlers.set(key, set);
      this.send({ t: "sub", sdk, events: [event] });
    }
    set.add(fn);
    this.#sync();
    return () => {
      if (!set.delete(fn) || set.size > 0) return;
      this.#handlers.delete(key);
      this.send({ t: "unsub", sdk, events: [event] });
      this.#sync();
    };
  }

  keepAlive(): () => void {
    this.#keep++;
    this.#sync();
    let released = false;
    return () => {
      if (released) return;
      released = true;
      this.#keep--;
      this.#sync();
    };
  }

  #need(): boolean {
    return this.#pending.size > 0 || this.#handlers.size > 0 || this.#keep > 0;
  }

  /** Keeps the event loop alive via stdin exactly when something expects a message. */
  #sync(): void {
    if (!this.#core || !this.#op) return;
    const want = this.#need();
    if (want && !this.#refed) this.#core.refOpPromise(this.#op);
    if (!want && this.#refed) this.#core.unrefOpPromise(this.#op);
    this.#refed = want;
  }

  async #readLoop(): Promise<void> {
    const buf = new Uint8Array(64 << 10);
    for (;;) {
      let n: number | null;
      try {
        if (this.#core) {
          const op = this.#core.read(0, buf);
          this.#op = op;
          this.#refed = true;
          this.#sync();
          n = await op;
          this.#op = undefined;
        } else {
          n = await Deno.stdin.read(buf);
        }
      } catch (e) {
        this.log("error", "tsbox: stdin: " + formatError(e));
        Deno.exit(70);
      }
      if (!n) Deno.exit(Deno.exitCode); // init closed stdin
      let frames: Frame[];
      try {
        frames = this.#dec.push(buf.slice(0, n));
      } catch (e) {
        this.log("error", "tsbox: protocol error from init: " + formatError(e));
        Deno.exit(70);
      }
      for (const f of frames) this.#handle(f);
    }
  }

  #handle(f: Frame): void {
    if (f.t === "ret") {
      const p = this.#pending.get(f.id ?? 0);
      if (!p) return;
      clearTimeout(p.timer);
      this.#pending.delete(f.id ?? 0);
      this.#sync();
      if (f.err) p.reject(new SdkError(f.err.code, f.err.msg));
      else p.resolve(f.p);
      return;
    }
    if (f.t === "event") {
      for (const fn of this.#handlers.get(`${f.sdk}:${f.name}`) ?? []) {
        try {
          fn(f.p);
        } catch (e) {
          this.log("error", `error in the handler for ${f.sdk}:${f.name}: ${formatError(e)}`);
        }
      }
    }
  }

  #redirectConsole(): void {
    const levels = { log: "info", info: "info", debug: "debug", warn: "warn", error: "error" } as const;
    for (const [fn, level] of Object.entries(levels)) {
      (console as unknown as Record<string, unknown>)[fn] = (...args: unknown[]) =>
        this.log(level, args.map((a) => typeof a === "string" ? a : Deno.inspect(a, { depth: 4 })).join(" "));
    }
  }

  /** Deno.stdout (and therefore node:process.stdout) ends up in the log line by line instead of in the protocol. */
  #redirectStdout(): void {
    const dec = new TextDecoder();
    const sink = (b: Uint8Array): number => {
      this.#stdoutLine += dec.decode(b, { stream: true });
      let i: number;
      while ((i = this.#stdoutLine.indexOf("\n")) >= 0) {
        this.log("info", this.#stdoutLine.slice(0, i));
        this.#stdoutLine = this.#stdoutLine.slice(i + 1);
      }
      if (this.#stdoutLine.length > MAX_LOG) {
        this.log("info", this.#stdoutLine);
        this.#stdoutLine = "";
      }
      return b.length;
    };
    const def = (k: string, v: PropertyDescriptor) => Object.defineProperty(Deno.stdout, k, v);
    def("writeSync", { value: sink });
    def("write", { value: (b: Uint8Array) => Promise.resolve(sink(b)) });
    def("writable", { get: () => new WritableStream<Uint8Array>({ write: (b) => void sink(b) }) });
    globalThis.addEventListener("unload", () => {
      if (this.#stdoutLine) this.log("info", this.#stdoutLine);
    });
  }
}

export function formatError(e: unknown): string {
  if (e instanceof Error) return e.stack ?? `${e.name}: ${e.message}`;
  return typeof e === "string" ? e : Deno.inspect(e);
}

/** The only instance per script process. */
export const transport: Transport = new Transport();
