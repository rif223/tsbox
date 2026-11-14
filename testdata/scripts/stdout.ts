// Deno.stdout and node:process.stdout end up in the log instead of in the protocol.
import process from "node:process";
Deno.stdout.writeSync(new TextEncoder().encode("A-writeSync\n"));
await Deno.stdout.write(new TextEncoder().encode("B-write\n"));
process.stdout.write("C-node\n");
