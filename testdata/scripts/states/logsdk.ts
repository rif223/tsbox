// Logs only through @tsbox/log (call frames, no log frames); ready() after 2 s.
import { ready } from "@tsbox/core";
import { log } from "@tsbox/log";
setInterval(() => log.info("tick"), 300);
setTimeout(() => ready(), 2000);
