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

- **kv on-disk format:** data now lives in `<state>/kv/<storage>/v2/<script>.json` with versions and expiry.
  Files of tsbox ≤ 0.3 (`<state>/kv/<storage>/<script>.json`) are migrated on first access and then removed, so
  an older tsbox no longer sees the data after an upgrade. Back up `<state>/kv` before upgrading if you might
  go back.

### Fixed

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
