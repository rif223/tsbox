// Import outside the build context (forbidden only in the build sandbox).
import { greet } from "../scripts/lib_outside.ts";
console.log(greet());
