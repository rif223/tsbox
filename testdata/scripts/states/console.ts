// Logs through console.log; ready() after 2 s.
import { ready } from "@tsbox/core";
setInterval(() => console.log("tick"), 300);
setTimeout(() => ready(), 2000);
