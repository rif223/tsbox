// Raw bytes bypassing Deno.stdout (node:fs on fd 1): protocol violation, init ends the script.
import fs from "node:fs";
fs.writeSync(1, "not a frame\n");
setTimeout(() => {}, 2000);
