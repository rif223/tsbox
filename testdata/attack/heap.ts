// Fill the V8 heap: reason "heap".
const a: string[][] = [];
setInterval(() => {
  for (let i = 0; i < 2000; i++) a.push(new Array(1000).fill("x" + i));
}, 1);
