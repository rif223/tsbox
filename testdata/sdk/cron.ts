import { cron } from "@tsbox/cron";

const info = await cron.schedules();
console.log("CRON schedules", info.schedules.map((s) => s.name).join(","), info.timezone);
let n = 0;
const off = cron.on("fast", (t) => {
  console.log("CRON tick", t.name, t.at >= t.scheduled);
  if (++n === 2) off();
});
