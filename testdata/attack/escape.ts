// I4: everything a script must NOT do. Every probe reports "PROBE <name> <result>"; an error is expected
// everywhere (NotCapable/PermissionDenied/…), never ALLOWED.
async function probe(name: string, fn: () => unknown | Promise<unknown>): Promise<void> {
  try {
    await fn();
    console.log(`PROBE ${name} ALLOWED`);
  } catch (e) {
    console.log(`PROBE ${name} ${(e as Error).name}`);
  }
}
const shell = Deno.build.os === "windows" ? "C:/Windows/System32/cmd.exe" : "/bin/sh";
await probe("env", () => Deno.env.get("PATH") ?? Deno.env.toObject());
await probe("read-root", () => Deno.readDirSync("/").next());
await probe("read-self-dir", () => Deno.readDirSync(new URL("./", import.meta.url)).next());
await probe("write", () => Deno.writeTextFileSync("pwned.txt", "x"));
await probe("net-tcp", () => Deno.connect({ hostname: "127.0.0.1", port: 80 }));
await probe("net-fetch", () => fetch("https://example.com"));
await probe("run", () => new Deno.Command(shell, { args: [] }).outputSync());
await probe("ffi", () => Deno.dlopen(Deno.build.os === "windows" ? "kernel32.dll" : "libc.so.6", {}));
await probe("sys", () => Deno.hostname());
await probe("node-fs", async () => (await import("node:fs")).readFileSync("/etc/hostname"));
await probe("node-child", async () => (await import("node:child_process")).execSync("echo x"));
await probe("node-env", async () => {
  const v = (await import("node:process")).default.env.PATH;
  if (v === undefined) throw new Error("empty");
});
const remote = ["https://example.com", "x.js"].join("/");
await probe("dyn-import-remote", () => import(remote));
const data = ["data:text/javascript", "export default Deno.env.get('PATH')"].join(",");
await probe("dyn-import-data", async () => {
  const m = await import(data);
  if (m.default === undefined) throw new Error("empty");
});
await probe("worker-data", () =>
  new Promise((res, rej) => {
    const w = new Worker("data:text/javascript,postMessage(Deno.env.get('PATH'))", { type: "module" });
    w.onmessage = (e) => (e.data === undefined ? rej(new Error("empty")) : res(e.data));
    w.onerror = (e) => {
      e.preventDefault();
      rej(new Error(e.message));
    };
  }));
await probe("wasm", async () => {
  // WASM itself is allowed (pure computation); the check is that it gets no host functions.
  const m = await WebAssembly.compile(new Uint8Array([0, 97, 115, 109, 1, 0, 0, 0]));
  if (WebAssembly.Module.imports(m).length === 0) throw new Error("no imports – ok");
});
console.log("PROBES DONE");
