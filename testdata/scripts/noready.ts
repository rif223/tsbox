// Keeps running but never reports ready() (the CPU boost at start ends only via the deadline).
import { keepAlive } from "@tsbox/core";
keepAlive();
console.log("without ready");
