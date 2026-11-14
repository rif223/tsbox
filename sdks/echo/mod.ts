/**
 * @tsbox/echo – template for an SDK with an external provider.
 *
 * ```ts
 * import { echo, onTick, tick } from "@tsbox/echo";
 * console.log(await echo("hello"));
 * const stop = onTick((n) => console.log("tick", n));
 * await tick(3);
 * stop();
 * ```
 * @module
 */
import { call, on } from "@tsbox/core";

/** Sends text to the provider and gets it back with a prefix. */
export async function echo(text: string): Promise<string> {
  return (await call<{ text: string }>("echo", "echo", { text })).text;
}

/** Lets the provider send n "tick" events (n ≤ grant echo:ticks, default 3). */
export async function tick(n: number): Promise<number> {
  return (await call<{ sent: number }>("echo", "tick", { n })).sent;
}

/** Subscribes to the tick events. Returns the unsubscribe function. */
export function onTick(fn: (n: number) => void): () => void {
  return on<{ n: number }>("echo", "tick", (p) => fn(p.n));
}
