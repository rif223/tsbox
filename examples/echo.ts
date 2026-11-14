// External provider with events.
//   tsbox run examples/echo.ts --sdk echo --grant echo:ticks=3
import { echo, onTick, tick } from "@tsbox/echo";

console.log(await echo("hello"));
const stop = onTick((n) => console.log("tick", n));
console.log("sent:", await tick(5));
stop();
