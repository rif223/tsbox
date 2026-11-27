/**
 * @tsbox/kv – key-value store. Every script has its own namespace; values are JSON.
 *
 * ```ts
 * import { kv } from "@tsbox/kv";
 * await kv.incr("visits");                                   // atomic counter
 * await kv.set("session:42", { user: 1 }, { ttl: 60_000 });  // expires after a minute
 *
 * // compare-and-set: retry until nobody else wrote in between
 * for (;;) {
 *   const e = await kv.getEntry<string[]>("list");
 *   const next = [...(e?.value ?? []), "item"];
 *   if (await kv.set("list", next, { ifVersion: e?.version ?? 0 })) break;
 * }
 * ```
 * Limits: key ≤ 256 bytes, value ≤ 64 KiB, quota 1 MiB per script (configurable by the operator). Expired
 * keys count as absent and no longer against the quota.
 * @module
 */
import { call } from "@tsbox/core";

/** A stored value with its metadata. */
export interface Entry<T = unknown> {
  value: T;
  /** Grows with every write of the key; use it with `ifVersion`. */
  version: number;
  /** End of the lifetime in milliseconds since the epoch, if the key has a TTL. */
  expiresAt?: number;
}

export interface SetOptions {
  /** Lifetime in milliseconds (1 ms to 10 years). Without it, the key never expires. */
  ttl?: number;
  /** Write only if the key has this version; 0 = only if the key does not exist yet. */
  ifVersion?: number;
}

export interface ListOptions {
  /** Only keys after this one (the last key of the previous page). */
  after?: string;
  /** At most this many keys (1–1,000; default 1,000). */
  limit?: number;
}

/** A page of keys. */
export interface KeyPage {
  keys: string[];
  /** True if more keys follow; continue with `after: keys[keys.length - 1]`. */
  more: boolean;
}

interface RawEntry<T> {
  found: boolean;
  value?: T;
  version?: number;
  expiresAt?: number;
}

export const kv = {
  /** Returns the value or undefined. */
  async get<T = unknown>(key: string): Promise<T | undefined> {
    const r = await call<RawEntry<T>>("kv", "get", { key });
    return r.found ? r.value : undefined;
  },
  /** Returns value, version and expiry, or undefined. */
  async getEntry<T = unknown>(key: string): Promise<Entry<T> | undefined> {
    const r = await call<RawEntry<T>>("kv", "get", { key });
    if (!r.found) return undefined;
    const e: Entry<T> = { value: r.value as T, version: r.version ?? 0 };
    if (r.expiresAt !== undefined) e.expiresAt = r.expiresAt;
    return e;
  },
  /**
   * Stores a JSON value. Returns false only if `ifVersion` did not match (nothing was written).
   */
  async set(key: string, value: unknown, opts: SetOptions = {}): Promise<boolean> {
    return (await call<{ ok: boolean }>("kv", "set", { key, value, ...opts })).ok;
  },
  /**
   * Adds `by` (default 1, may be negative) to an integer value atomically and returns the new value. A missing
   * key starts at 0; `ttl` applies only when the key is created.
   */
  async incr(key: string, by = 1, opts: { ttl?: number } = {}): Promise<number> {
    return (await call<{ value: number }>("kv", "incr", { key, by, ...opts })).value;
  },
  /** Deletes a key; true if it existed (and, with `ifVersion`, had that version). */
  async delete(key: string, opts: { ifVersion?: number } = {}): Promise<boolean> {
    return (await call<{ deleted: boolean }>("kv", "delete", { key, ...opts })).deleted;
  },
  /** Lists keys (sorted, at most 1,000), optionally with a prefix. For more, use listPage. */
  async list(prefix = ""): Promise<string[]> {
    return (await call<KeyPage>("kv", "list", { prefix })).keys;
  },
  /** Lists one page of keys with the prefix. */
  async listPage(prefix = "", opts: ListOptions = {}): Promise<KeyPage> {
    const r = await call<KeyPage>("kv", "list", { prefix, ...opts });
    return { keys: r.keys, more: r.more ?? false };
  },
  /** Iterates over all keys with the prefix, page by page. */
  async *keys(prefix = ""): AsyncGenerator<string> {
    let after: string | undefined;
    for (;;) {
      const page: KeyPage = await kv.listPage(prefix, after === undefined ? {} : { after });
      yield* page.keys;
      if (!page.more || page.keys.length === 0) return;
      after = page.keys[page.keys.length - 1];
    }
  },
};
