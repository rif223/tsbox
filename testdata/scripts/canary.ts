// Canary test (I2): keeps the passed marker in memory (positive control of the memory scan), only asks for
// the length of the secret and stays alive until the test stops the box.
import { keepAlive, ready, script } from "@tsbox/core";
import { secretLength } from "@test/secretlen";
const marker = String((script.params as { marker: string }).marker);
(globalThis as Record<string, unknown>).tsboxMarker = [marker, marker + "-kopie"];
console.log(`secret-len ${await secretLength()}`);
keepAlive();
ready();
