// Fetches a granted API.
//   tsbox run examples/fetch.ts --sdk http --grant http:host=api.github.com
import { fetch } from "@tsbox/http";

const r = await fetch("https://api.github.com/repos/denoland/deno");
const repo = r.json<{ full_name: string; stargazers_count: number }>();
console.log(`${repo.full_name}: ${repo.stargazers_count} stars (HTTP ${r.status})`);
