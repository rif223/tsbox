import { fetch } from "@tsbox/http";
import { script, SdkError } from "@tsbox/core";

const { url, other } = script.params as { url: string; other: string };
const r = await fetch(url);
console.log("HTTP", r.status, r.body);
for (const u of [other, "file:///etc/passwd"]) {
  try {
    await fetch(u);
    console.log("HTTP ALLOWED", u);
  } catch (e) {
    console.log("HTTP denied", (e as SdkError).code);
  }
}
