// A timer keeps the script alive; afterwards it ends by itself (stdin is unref'd then).
setTimeout(() => console.log("later"), 300);
