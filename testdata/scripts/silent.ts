// Keeps running, never writes anything and never reports ready() (CPU boost: ends only via the deadline).
import { keepAlive } from "@tsbox/core";
keepAlive();
