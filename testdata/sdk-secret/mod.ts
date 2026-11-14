/** Test SDK for the canary test: returns only the length of the secret. */
import { call } from "@tsbox/core";

/** Length of the configured secret. */
export async function secretLength(): Promise<number> {
  return (await call<{ n: number }>("secretlen", "len", {})).n;
}
