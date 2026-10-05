# Operating GoSync

This guide covers running GoSync in production: deployment, configuration, scaling,
monitoring, backups, upgrades and capacity.

## Deployment checklist

- [ ] Serve over HTTPS so clients connect with `wss://`. Terminate TLS at your load balancer or set `GOSYNC_TLS_CERT`/`GOSYNC_TLS_KEY`.
- [ ] Set `GOSYNC_ALLOWED_ORIGINS` to your app's origins.
- [ ] Use JWKS (`GOSYNC_JWKS_URL`) or a random secret of at least 32 bytes (`GOSYNC_JWT_SECRET`), plus `GOSYNC_JWT_ISSUER` and `GOSYNC_JWT_AUDIENCE`.
- [ ] Make sure `GOSYNC_INSECURE_DEV_AUTH` is **not** set.
- [ ] Use PostgreSQL for anything beyond a single small server.
- [ ] Bind `GOSYNC_METRICS_ADDR` to a private interface and scrape it with Prometheus.
- [ ] Back up the database (see below).
- [ ] Serve `gosync.wasm` with its precompressed `.br`/`.gz` siblings (about 0.8 MB instead of 4 MB).
- [ ] Configure your load balancer for WebSockets: allow upgrades, set an idle timeout above 60 s, and use `/readyz` for health checks.

## Commands

```bash
gosync-server                         # run the server
gosync-server migrate                 # apply schema migrations and exit (run before rolling out a new version)
gosync-server compact -older-than 720h  # purge old tombstones once and exit
```

## Configuration

All configuration comes from environment variables; the README lists them. These ones affect operations:

| Variable | Default | Notes |
|---|---|---|
| `GOSYNC_DB_MAX_CONNS` | 25 | PostgreSQL pool per instance. Keep `instances × this` below `max_connections`. |
| `GOSYNC_TOMBSTONE_RETENTION` | `720h` | Deleted documents are compacted after this long. Minimum 24h; `0` disables compaction. |
| `GOSYNC_METRICS_ADDR` | off | Private listener serving `/metrics` (Prometheus) and `/debug/pprof/`. |
| `GOSYNC_LOG_FORMAT`, `GOSYNC_LOG_LEVEL` | `json`, `info` | Structured logs. Each session logs its subject and client ID; tokens are never logged. |

## Scaling

- **One instance:** SQLite or PostgreSQL. SQLite needs a persistent volume and must not be shared between instances.
- **Several instances:** PostgreSQL is required. Instances find each other's changes through `LISTEN/NOTIFY` (batched every 10 ms), so a user's devices can be connected to different instances. Any load balancer works and sticky sessions are not needed.
- **Connection limit:** each instance accepts up to 10,000 sockets by default. Rate-limit new connections per IP at your load balancer.

## Monitoring

Key metrics on `GOSYNC_METRICS_ADDR`:

| Metric | Watch for |
|---|---|
| `gosync_sessions` | Connected devices |
| `gosync_request_duration_seconds{op="push"\|"pull"}` | p99 rising means database pressure |
| `gosync_auth_failures_total` | Spikes mean misconfigured auth or an attack |
| `gosync_mutations_rejected_total` | Clients with bad clocks or buggy writers |
| `gosync_errors_total{code="internal"}` | Should stay at 0; check the logs |
| `gosync_tombstones_compacted_total` | Compaction is running |
| `process_resident_memory_bytes`, `go_goroutines` | About 2 goroutines and ~120 KiB per connection is normal |

Readiness: `/readyz` returns 503 when the database is unreachable.

## Backups and restore

- **PostgreSQL:** use your provider's point-in-time recovery or `pg_dump`. All data lives in the `gosync_*` tables.
- **SQLite:** `sqlite3 gosync.db ".backup backup.db"` makes a consistent online copy. Don't copy the file while the server runs unless you also copy `-wal` from the same instant.
- **Restoring an older backup keeps devices consistent from then on.** Namespace versions are derived from the clock, so versions issued after a restore are always above cursors handed out before it, and no device skips new writes. Writes still in device outboxes are pushed again.
- **What a restore can't recover:** writes that existed only on the server after the backup are lost from the server. A device that had already downloaded them keeps its copy until that document is written again.

## Upgrades

1. Run `gosync-server migrate` with the new binary. Migrations are transactional, and on PostgreSQL concurrent runs are serialised by an advisory lock.
2. Roll instances one by one. On shutdown the server sends a `going away` close frame, and clients reconnect to another instance with jittered backoff.
3. A server refuses to start on a schema newer than it understands. To roll back, deploy the previous binary only if no new migration ran.

## Tombstones and long-offline devices

Deleting a document leaves a small tombstone so other devices learn about the delete. After `GOSYNC_TOMBSTONE_RETENTION`, compaction replaces it with an even smaller purge record:

- Devices that already had the document remove it when they next pull.
- A write made *before* the delete, arriving late from a device that was offline, can't bring the document back.
- A write made *after* the delete legitimately revives it.

## Capacity (measured)

Measured with `cmd/gosync-loadtest`. Each device writes about 200-byte values; latency is the time from a write on one device until it's readable on that user's other devices.

| Setup | Devices | Writes/s | p50 | p95 | p99 | max |
|---|---|---|---|---|---|---|
| SQLite, 1 instance, 12-thread laptop (load generator on the same machine) | 1,000 | 1,022 | 2.7 ms | 26 ms | 81 ms | 225 ms |
| same | 2,000 | 1,048 | 2.8 ms | 28 ms | 92 ms | 274 ms |
| same, past saturation | 1,000 | 2,012 | 2.0 s | 6.4 s | 7.1 s | 8.7 s |
| PostgreSQL 17, 2 instances, 4-vCPU CI runner (DB, servers and load generator on one machine) | 600 | 611 | 18 ms | 44 ms | 140 ms | 405 ms |

Every run delivered 100% of writes with zero errors, including past saturation, where the system slows down but loses nothing. Server memory was about 125 MiB at 1,000 connections. CI runs the PostgreSQL scenario on every push and fails if p99 exceeds 1 s or any delivery is missing.

Reproduce:

```bash
GOSYNC_INSECURE_DEV_AUTH=true GOSYNC_METRICS_ADDR=127.0.0.1:9090 go run ./cmd/gosync-server &
go run ./cmd/gosync-loadtest -url ws://127.0.0.1:8080/sync -users 250 -devices 4 -interval 1s -duration 60s \
  -metrics http://127.0.0.1:9090/metrics
```

Run the load generator on a different machine from the server for numbers that reflect your hardware.
