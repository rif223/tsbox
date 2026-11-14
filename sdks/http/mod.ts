/**
 * @tsbox/http – fetch to granted hosts (--grant http:host=api.github.com).
 *
 * ```ts
 * import { fetch } from "@tsbox/http";
 * const r = await fetch("https://api.github.com/repos/denoland/deno");
 * console.log(r.status, r.json<{ stargazers_count: number }>().stargazers_count);
 * ```
 * https only (unless the operator allows http), no private addresses, response ≤ 1 MiB.
 * @module
 */
import { call } from "@tsbox/core";

export interface FetchInit {
  method?: "GET" | "HEAD" | "POST" | "PUT" | "PATCH" | "DELETE";
  headers?: Record<string, string>;
  body?: string;
}

export interface FetchResponse {
  readonly status: number;
  readonly ok: boolean;
  readonly url: string;
  readonly headers: Record<string, string>;
  readonly body: string;
  json<T = unknown>(): T;
}

interface Raw {
  status: number;
  url: string;
  headers: Record<string, string>;
  body: string;
}

/** Fetches a URL. Errors: permission_denied (host not granted), too_large, timeout, unavailable. */
export async function fetch(url: string, init: FetchInit = {}): Promise<FetchResponse> {
  const r = await call<Raw>("http", "fetch", {
    url,
    method: init.method ?? "GET",
    headers: init.headers,
    body: init.body,
  });
  return {
    ...r,
    ok: r.status >= 200 && r.status < 300,
    json: <T>() => JSON.parse(r.body) as T,
  };
}
