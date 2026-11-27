import { kv } from "@tsbox/kv";

console.log("KV2 incr", await kv.incr("n"), await kv.incr("n", 4));
console.log("KV2 create-only", await kv.set("c", 1, { ifVersion: 0 }), await kv.set("c", 2, { ifVersion: 0 }));
const e = await kv.getEntry<number>("c");
console.log(
  "KV2 cas",
  await kv.set("c", 3, { ifVersion: e!.version }),
  await kv.set("c", 4, { ifVersion: e!.version }),
);
await kv.set("t", "x", { ttl: 1 });
await new Promise((r) => setTimeout(r, 20));
console.log("KV2 ttl", await kv.get("t"));
for (let i = 0; i < 5; i++) await kv.set("p" + i, i);
const page = await kv.listPage("p", { limit: 2 });
const all: string[] = [];
for await (const k of kv.keys("p")) all.push(k);
console.log("KV2 page", page.keys.join(","), page.more, "all", all.join(","));
