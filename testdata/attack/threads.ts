// I6: create workers until the thread limit applies.
const ws: Worker[] = [];
const src = URL.createObjectURL(new Blob(["self.postMessage(1)"], { type: "text/javascript" }));
for (let i = 0; i < 200; i++) {
  try {
    const w = new Worker(src, { type: "module" });
    await new Promise((r, j) => {
      w.onmessage = r;
      w.onerror = (e) => {
        e.preventDefault();
        j(e);
      };
    });
    ws.push(w);
  } catch (e) {
    console.log(`WORKERS ${ws.length} ${String(e).slice(0, 80)}`);
    break;
  }
}
console.log(`WORKERS ${ws.length} done`);
for (const w of ws) w.terminate();
