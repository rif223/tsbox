// Rate limit, concurrency and the oracle ban (same answer for an unknown SDK and an unknown method).
import { call, SdkError } from "@tsbox/core";
import { kv } from "@tsbox/kv";

const codes = async (p: Promise<unknown>[]) =>
  (await Promise.allSettled(p)).map((r) => r.status === "fulfilled" ? "ok" : (r.reason as SdkError).code);
// kv.list (burst 40, 20/s): 300 calls empty the bucket reliably. With kv.get (burst 400) the result only
// depended on whether more than 16 calls happened to be open at the same time.
const many = await codes(Array.from({ length: 300 }, () => kv.list("x")));
console.log(
  "LIMIT rate",
  many.filter((c) => c === "rate_limited").length > 0,
  many.filter((c) => c === "ok").length > 0,
);
const unknownSdk = await codes([call("nosuch", "get", {})]);
const unknownMethod = await codes([call("kv", "nosuch", {})]);
console.log("LIMIT oracle", unknownSdk[0], unknownMethod[0]);
const big = await codes([call("kv", "get", { key: "x".repeat(4096) })]);
console.log("LIMIT params", big[0]);
