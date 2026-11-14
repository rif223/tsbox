/**
 * Included by the build as the first module of every script (before the script code): console, stdout,
 * error handlers and heartbeat are therefore always active, even in scripts without imports.
 * @module
 */
import { transport } from "./transport.ts";

transport.start();
