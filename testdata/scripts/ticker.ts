// Writes "tick <n>" every 100 ms; params.n limits the count (default: unlimited).
import { keepAlive, ready, script } from "@tsbox/core";
const n = (script.params as { n?: number } | null)?.n ?? Infinity;
keepAlive();
ready();
let i = 0;
const t = setInterval(() => {
  i++;
  console.log(`tick ${i}`);
  if (i >= n) {
    clearInterval(t);
    Deno.exit(0);
  }
}, 100);
