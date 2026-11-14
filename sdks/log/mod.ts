/**
 * @tsbox/log – structured logs. Fields are appended to the line as JSON.
 *
 * ```ts
 * import { log } from "@tsbox/log";
 * log.info("order created", { id: 42 });
 * ```
 * @module
 */
import { call } from "@tsbox/core";

/** Additional fields of a log line. */
export type Fields = Record<string, string | number | boolean | null>;

function write(level: "debug" | "info" | "warn" | "error", msg: string, fields?: Fields): void {
  call("log", "write", { level, msg, fields }).catch(() => {});
}

export const log = {
  debug: (msg: string, fields?: Fields): void => write("debug", msg, fields),
  info: (msg: string, fields?: Fields): void => write("info", msg, fields),
  warn: (msg: string, fields?: Fields): void => write("warn", msg, fields),
  error: (msg: string, fields?: Fields): void => write("error", msg, fields),
};
