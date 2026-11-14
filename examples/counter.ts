// Counts runs persistently with @tsbox/kv: every run increments the counter.
//   tsbox run examples/counter.ts --sdk kv
import { kv } from "@tsbox/kv";
import { log } from "@tsbox/log";

const n = ((await kv.get<number>("runs")) ?? 0) + 1;
await kv.set("runs", n);
log.info("run counted", { run: n, keys: (await kv.list()).length });
