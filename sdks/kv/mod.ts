/**
 * @tsbox/kv – key-value store. Every script has its own namespace; values are JSON.
 *
 * ```ts
 * import { kv } from "@tsbox/kv";
 * await kv.set("counter", ((await kv.get<number>("counter")) ?? 0) + 1);
 * ```
 * Limits: key ≤ 256 bytes, value ≤ 64 KiB, quota 1 MiB per script (configurable by the operator).
 * @module
 */
import { call } from "@tsbox/core";

export const kv = {
  /** Returns the value or undefined. */
  async get<T = unknown>(key: string): Promise<T | undefined> {
    const r = await call<{ found: boolean; value?: T }>("kv", "get", { key });
    return r.found ? r.value : undefined;
  },
  /** Stores a JSON value. */
  async set(key: string, value: unknown): Promise<void> {
    await call("kv", "set", { key, value });
  },
  /** Deletes a key; true if it existed. */
  async delete(key: string): Promise<boolean> {
    return (await call<{ deleted: boolean }>("kv", "delete", { key })).deleted;
  },
  /** Lists keys (sorted, at most 1,000), optionally with a prefix. */
  async list(prefix = ""): Promise<string[]> {
    return (await call<{ keys: string[] }>("kv", "list", { prefix })).keys;
  },
};
