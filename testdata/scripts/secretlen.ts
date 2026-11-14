// Reports the length of the secret (never the value) and stays alive afterwards.
import { keepAlive, ready } from "@tsbox/core";
import { secretLength } from "@test/secretlen";
console.log(`secret-len ${await secretLength()}`);
keepAlive();
ready();
