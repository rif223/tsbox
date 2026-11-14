// I6: allocates native memory outside the V8 heap (ArrayBuffer) until the script limit applies.
const keep: Uint8Array[] = [];
setInterval(() => {
  const b = new Uint8Array(16 << 20);
  b.fill(1);
  keep.push(b);
  console.log(`allocated ${keep.length * 16} MiB`);
}, 100);
