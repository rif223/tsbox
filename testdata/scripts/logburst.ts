// 1000 lines at once: the log rate limit (20/s, burst 100) lets 100 through.
for (let i = 0; i < 1000; i++) console.log("line " + i);
