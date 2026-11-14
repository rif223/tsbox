/**
 * Frame protocol v2 (u32 big endian length | JSON), identical to internal/proto in Go.
 * @module
 */

export const MAX_FRAME = 1 << 20;

export interface FrameError {
  code: string;
  msg?: string;
}

export interface Frame {
  t: string;
  sid?: string;
  id?: number;
  sdk?: string;
  m?: string;
  name?: string;
  events?: string[];
  level?: string;
  msg?: string;
  p?: unknown;
  err?: FrameError;
}

const encoder = new TextEncoder();
const decoder = new TextDecoder();

/** Encodes a frame including the length prefix. */
export function encodeFrame(f: Frame): Uint8Array {
  const body = encoder.encode(JSON.stringify(f));
  if (body.length > MAX_FRAME) throw new RangeError("frame too large");
  const out = new Uint8Array(4 + body.length);
  new DataView(out.buffer).setUint32(0, body.length);
  out.set(body, 4);
  return out;
}

/** Reassembles frames from arbitrarily split bytes. Throws on protocol errors. */
export class FrameDecoder {
  #buf = new Uint8Array(0);

  push(chunk: Uint8Array): Frame[] {
    const next = new Uint8Array(this.#buf.length + chunk.length);
    next.set(this.#buf);
    next.set(chunk, this.#buf.length);
    this.#buf = next;
    const out: Frame[] = [];
    while (this.#buf.length >= 4) {
      const n = new DataView(this.#buf.buffer, this.#buf.byteOffset, 4).getUint32(0);
      if (n === 0 || n > MAX_FRAME) throw new Error(`invalid frame length ${n}`);
      if (this.#buf.length < 4 + n) break;
      const f = JSON.parse(decoder.decode(this.#buf.subarray(4, 4 + n)));
      if (typeof f !== "object" || f === null || typeof f.t !== "string") throw new Error("invalid frame");
      this.#buf = this.#buf.subarray(4 + n);
      out.push(f as Frame);
    }
    return out;
  }

  get pending(): number {
    return this.#buf.length;
  }
}

/** Writes synchronously and completely (frames stay atomic and ordered). */
export function writeAllSync(write: (p: Uint8Array) => number, data: Uint8Array): void {
  let off = 0;
  while (off < data.length) off += write(data.subarray(off));
}
