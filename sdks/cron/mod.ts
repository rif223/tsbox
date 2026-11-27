/**
 * @tsbox/cron – run code on schedules the operator grants.
 *
 * ```ts
 * // tsbox run job.ts --sdk cron --grant 'cron:schedule=nightly=0 3 * * *' --grant 'cron:schedule=@every 5m'
 * import { cron } from "@tsbox/cron";
 * cron.on("nightly", async () => { await cleanup(); });
 * cron.on("@every 5m", (t) => console.log("late by", t.at - t.scheduled, "ms"));
 * ```
 * Expressions have 5 fields (minute hour day month weekday) with *, lists, ranges, steps and names, or
 * @hourly, @daily, @weekly, @monthly, @yearly, @every <duration> (at least 1s). The time zone is the
 * operator's configuration (default UTC). A subscription keeps the script running; runs missed while the
 * host slept are not repeated.
 * @module
 */
import { call, on } from "@tsbox/core";

/** A due schedule. */
export interface Tick {
  /** Name of the schedule (the part before "=" in the grant, otherwise the expression). */
  name: string;
  schedule: string;
  /** When it was due and when the event was sent, in milliseconds since the epoch. */
  scheduled: number;
  at: number;
}

/** A granted schedule with its next run. */
export interface ScheduleInfo {
  name: string;
  schedule: string;
  next?: number;
}

export const cron = {
  /** Calls handler for every tick of one schedule (by name), or of all schedules with "*". */
  on(name: string, handler: (t: Tick) => void | Promise<void>): () => void {
    return on<Tick>("cron", "tick", (t) => {
      if (name === "*" || t.name === name) {
        Promise.resolve(handler(t)).catch((e) => console.error(`cron ${t.name}:`, e));
      }
    });
  },
  /** The granted schedules and their next runs. */
  async schedules(): Promise<{ schedules: ScheduleInfo[]; timezone: string }> {
    return await call("cron", "next", {});
  },
};
