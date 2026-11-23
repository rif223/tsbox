// Never writes anything; ready() after 2 s.
import { ready } from "@tsbox/core";
setInterval(() => {}, 300);
setTimeout(() => ready(), 2000);
