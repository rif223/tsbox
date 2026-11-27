import { keepAlive, ready, script } from "@tsbox/core";
keepAlive();
ready();
console.log("VERSION", JSON.stringify(script.params));
