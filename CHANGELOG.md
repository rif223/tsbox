# Changelog

All notable changes to tsbox. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versions are the tags `v<version>` (see CONTRIBUTING.md, Releases).

## [Unreleased]

### Added

- **@tsbox/kv v2:** versions per key with compare-and-set (`set(key, value, { ifVersion })`,
  `delete(key, { ifVersion })`, `getEntry`), atomic counters (`incr`), expiry (`ttl` in milliseconds) and paged
  listing (`listPage` with `after`/`limit`, `keys()` as an async generator).
- **Pluggable kv storage:** the public interface `kvstore.Store` and the option `tsbox.WithKVStore`, e.g. for a
  store in PostgreSQL. The default stays `kvstore.FileStore` below `<state>/kv`.
- **@tsbox/inbox:** messages between host and script. The host sends with `tsbox emit <box> <topic> [json]`,
  `POST /v1/boxes/{name}/emit` or `Box.Emit`; the script answers with `inbox.send`, which becomes the engine event
  `script_message` (`tsbox events`, `WithEvents`). `tsbox emit --event <sdk>:<event>` sends a raw event of any SDK.
- **@tsbox/cron:** schedules granted by the operator (`--grant 'cron:schedule=nightly=0 3 * * *'`) become `tick`
  events: 5-field expressions with lists, ranges, steps and names, the macros `@hourly` … `@yearly` and
  `@every <duration>` (at least 1s), time zone from the SDK configuration (default UTC). Invalid schedules are
  refused when the box is created (new optional provider interface `sdk.GrantValidator`).
- **Updating a box in place:** `tsbox update [--spec box.json | --mem --cpu --pids --entry --params] <box>`,
  `PUT /v1/boxes/{name}` and `Engine.Update`. The new specification is built and checked while the old box keeps
  running; only then the boxes are swapped. A build, check or quota error leaves the old box untouched; if the new
  box does not start, the old specification is started again. Name, storage and log files stay.
- **Metrics:** `GET /v1/metrics` (Prometheus text format) on the socket, and opt-in
  `tsbox daemon --metrics-addr 127.0.0.1:9464`, which serves only `GET /metrics` over TCP. Boxes by state, limits,
  memory, CPU, PIDs, OOM kills, script restarts, and `tsbox_sdk_calls_total` by box, SDK, method and result
  (`Engine.SDKCalls`).
- **Tenant quotas:** `tsbox daemon --tenant-label tenant --tenant-quota boxes=10,memory=2Gi,cpu=4,pids=1024`
  (`WithTenantQuotas`, `ParseQuota`) limits the boxes of every tenant together: the number of boxes always, memory,
  CPU and PIDs of the running ones. Usage under `GET /v1/tenants` (`Engine.Tenants`) and in the metrics; the error
  code is `quota` (HTTP 409).
- `tsbox run --label key=value` (repeatable). Keys starting with `tsbox.` are reserved for projects.
- Fuzz targets for cron expressions and tenant quotas.

### Changed

- **Box cgroups per engine:** gVisor and Firecracker put boxes below `/sys/fs/cgroup/tsbox/<8 hex digits of the
  state directory>/<box>` instead of `/sys/fs/cgroup/tsbox/<box>`; `Engine.Close` removes the parent once it is
  empty.
- **gVisor start ~1 s faster:** init measures the RSS baseline of an empty Deno only at the first start of a
  rootfs and init binary; later starts get the remembered value (`<state>/baseline-rss.json`). Until init
  ready: 1.3 s → 0.26 s; one script ready: 1.67 s → 0.62 s.
- **External SDK providers** end one minute after their last session (`extproc.Config.IdleTimeout`) instead of
  running until the engine closes; the next use starts them again.

- **kv on-disk format:** data now lives in `<state>/kv/<storage>/v2/` as a snapshot `<script>.json` (versions and
  expiry) plus a journal `<script>.log`: a write appends one synced line instead of rewriting the namespace
  (200 small writes to a full 1 MiB namespace: 2 s → 0.24 s), the snapshot is rewritten atomically and synced
  once the journal is larger. A torn last journal line is dropped; a damaged snapshot is moved aside
  (`.damaged-<time>`) instead of blocking the namespace.
  Files of tsbox ≤ 0.3 (`<state>/kv/<storage>/<script>.json`) are migrated on first access and then removed, so
  an older tsbox no longer sees the data after an upgrade. Back up `<state>/kv` before upgrading if you might
  go back.

### Fixed

- Two engines or daemons with different state directories shared the cgroup of a box name: under gVisor the
  second box ran with the limits of the first, and killing one ended both; under Firecracker the second did not
  start.
- `Box.Stop` while the box was starting returned "does not end" after 20 s, and the box then ran anyway. Stop now
  ends a starting box (Start returns "stopped while starting").
- kv: no fsync before replacing the file; a damaged file made the namespace unusable.
- An external provider got two `close` frames for one session on some paths; a session whose open timed out
  stayed registered.
- `Box.Wait` called between `Create` and `Start` never returned.
- Script params were not checked: above ~32 KiB (Windows) or 128 KiB (Linux) the start failed with `spawn_failed`,
  above 1 MiB the script stayed `starting` for ever. Params must now be valid JSON of at most 64 KiB
  (`MaxParams`; 8 KiB with the dev driver on Windows, `MaxParamsWindows`).
- Log lines over the rate limit (20/s, burst 100 per script) vanished without a trace. They are now counted
  (`ScriptStatus.DroppedLogLines`, metric `tsbox_script_log_lines_dropped_total`) and reported by a line
  "tsbox: N log lines dropped".
- SDK rate limits started with a full burst at every script restart (`--restart always` doubled the allowed
  rate); the buckets now belong to the script.
- The watchdog measured the box while holding its lock (under Firecracker a vsock round trip of up to 3 s that
  blocked the box's API); the OOM check at a killed script is bounded to 1 s.
- Build sandbox: an import outside the script's directory failed with "Type error in the script"; it now gets
  its own hint. SECURITY.md names what a build reads as a known limit.
- http: the SSRF check missed several special-purpose ranges (0.0.0.0/8, 198.18.0.0/15, 240.0.0.0/4, documentation
  ranges, Teredo, local-use NAT64, …) and NAT64/6to4 addresses that embed a private IPv4 address
  (`64:ff9b::7f00:1` = 127.0.0.1).
- http: a response below `maxBody` that does not fit an SDK result as JSON (escaped body plus headers, at most
  1020 KiB) now fails with a message that says so instead of "result larger than 1020Ki".
- dev driver: one leaked file descriptor per box whose start failed while setting limits or `oom_score_adj`.
- Messages of canceled SDK calls ("abgebrochen") and provider panics were German.
- Docs: SECURITY.md called gvisor the default driver; `Box.Emit` said a full queue drops the event per script
  (it is dropped for all recipients).
- A CPU limit of NaN passed `Limits.Validate` (every comparison with NaN is false; reachable through the Go API,
  not through tsboxd, since JSON cannot carry NaN). With tenant quotas it also turned the CPU sum into NaN, so
  every later CPU quota check passed.
- Data race in the test `TestInterrupt` (it read the child's output while `os/exec` was writing it).

Fixed before release in the features above:

- `WithKVStore` had no effect: the engine always created the default `FileStore`.
- `Engine.Update` checked only the number of boxes before stopping the old box; an update beyond the memory, CPU
  or PID quota stopped the old box and brought it back only through the rollback (a restart).
- After a failed update, tsboxd kept pointing at the stopped old box while the engine ran the rollback box:
  `tsbox ps` showed the box stopped, and `stop`/`rm` did not reach the running one.
- Concurrent `Engine.Create` calls of one tenant could all pass the box quota (the count was taken outside the
  engine lock). tsboxd serialized creates itself; the library API did not.
- `--tenant-quota cpu=nan` (or `inf`) was accepted and meant "unlimited".
- The example in the docs of `@tsbox/inbox` sent the topic `inbox:message` instead of `ping`.
