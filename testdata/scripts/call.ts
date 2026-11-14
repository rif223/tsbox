// Without an SDK the host answers not_found; afterwards the script runs to its end normally.
import { call, SdkError } from "@tsbox/core";
try {
  await call("kv", "get", { key: "x" });
  console.log("CALL ALLOWED");
} catch (e) {
  console.log("CALL", e instanceof SdkError ? e.code : String(e));
}
