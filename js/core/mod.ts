/**
 * @tsbox/core – basic functions of every script in a tsbox box.
 *
 * ```ts
 * import { call, keepAlive, on, script } from "@tsbox/core";
 * console.log("I am", script.name);
 * ```
 * SDKs (e.g. @tsbox/kv) build on `call` and `on`.
 * @module
 */
import { transport } from "./transport.ts";

export { SdkError } from "./transport.ts";

/** Name and (non-secret) parameters of this script. */
export const script: { readonly name: string; readonly params: unknown } = transport.script;

/** Calls a method of an SDK. Errors arrive as SdkError (code: not_found, permission_denied, …). */
export function call<T = unknown>(sdk: string, method: string, params?: unknown, timeoutMs?: number): Promise<T> {
  return transport.call(sdk, method, params, timeoutMs) as Promise<T>;
}

/** Subscribes to an SDK event. As long as a subscription exists, the script keeps running. Returns the unsubscribe function. */
export function on<T = unknown>(sdk: string, event: string, handler: (payload: T) => void): () => void {
  return transport.on(sdk, event, handler as (p: unknown) => void);
}

/** Keeps the script alive until the returned function is called (e.g. for services). */
export function keepAlive(): () => void {
  return transport.keepAlive();
}

/** Reports "ready" to the host (event ScriptReady). */
export function ready(): void {
  transport.ready();
}
