/**
 * @tsbox/inbox – messages between the host and the script.
 *
 * ```ts
 * import { inbox } from "@tsbox/inbox";
 * inbox.on((msg) => {                          // tsbox emit bot ping
 *   if (msg.topic === "ping") inbox.send("pong", { at: Date.now() });
 * });
 * ```
 * Incoming messages come from whoever controls tsboxd (CLI, API, embedding application): `tsbox emit`,
 * `POST /v1/boxes/{name}/emit` or `Box.Emit`. Outgoing messages appear as events of kind `script_message`
 * (`tsbox events`, `WithEvents`). A subscription keeps the script running.
 * @module
 */
import { call, on } from "@tsbox/core";

/** A message from the host. */
export interface Message<T = unknown> {
  topic: string;
  data?: T;
}

export const inbox = {
  /** Subscribes to messages from the host; returns the function that ends the subscription. */
  on<T = unknown>(handler: (msg: Message<T>) => void): () => void {
    return on<Message<T>>("inbox", "message", handler);
  },
  /** Sends a message to the host (topic: 1–64 characters, data: JSON up to 16 KiB). */
  async send(topic: string, data?: unknown): Promise<void> {
    await call("inbox", "send", { topic, data });
  },
};
