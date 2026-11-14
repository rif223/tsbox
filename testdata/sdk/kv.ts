import { kv } from "@tsbox/kv";
import { SdkError } from "@tsbox/core";

const before = await kv.get<number>("n");
await kv.set("n", (before ?? 0) + 1);
await kv.set("obj", { a: [1, 2] });
console.log("KV n", await kv.get("n"), "obj", JSON.stringify(await kv.get("obj")), "keys", (await kv.list()).join(","));
console.log("KV deleted", await kv.delete("obj"), await kv.delete("obj"));
try {
  await kv.set("big", "x".repeat(200 * 1024));
} catch (e) {
  console.log("KV big", (e as SdkError).code);
}
try {
  for (let i = 0; i < 100; i++) await kv.set("q" + i, "x".repeat(1000));
  console.log("KV quota ALLOWED");
} catch (e) {
  console.log("KV quota", (e as SdkError).code);
}
