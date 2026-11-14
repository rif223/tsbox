// Load test: many kv calls and log lines per script, then exit with code 0. params.n = rounds.
import { kv } from "@tsbox/kv";
import { script } from "@tsbox/core";
const n = (script.params as { n?: number } | null)?.n ?? 40;
let sum = 0;
for (let i = 1; i <= n; i++) {
  await kv.set("i", i);
  const v = await kv.get<number>("i");
  if (v !== i) throw new Error(`kv: ${v} instead of ${i}`);
  sum += v;
  console.log(`LOAD ${script.name} ${i}`);
}
console.log(`LOAD ${script.name} done ${sum}`);
