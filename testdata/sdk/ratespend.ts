// Spends the burst of http.fetch (20) and fails, so that the restart policy starts it again.
import { fetch } from "@tsbox/http";

let passed = 0;
for (let i = 0; i < 30; i++) {
  try {
    await fetch("https://127.0.0.1/");
    passed++;
  } catch (e) {
    if ((e as { code?: string }).code !== "rate_limited") passed++;
  }
}
console.log("RATE passed " + passed);
Deno.exit(1);
