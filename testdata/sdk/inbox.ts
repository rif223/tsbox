import { ready } from "@tsbox/core";
import { inbox } from "@tsbox/inbox";

const off = inbox.on<{ n: number }>((msg) => {
  console.log("INBOX got", msg.topic, JSON.stringify(msg.data));
  inbox.send("pong", { n: (msg.data?.n ?? 0) + 1 }).then(() => {
    if (msg.topic === "bye") off();
  });
});
ready();
