---
tags:
  - Observability
  - Operations
---

# Observability

sabon's observability comes in two forms: a built-in HTTP server exposing
Prometheus **metrics** and **health** endpoints (below), and push
**[notifications](#notifications)** on each run's outcome. The server listens on
`SABON_METRICS_ADDR` (default `:9333`; blank disables it) — bind it to loopback
or a private interface and scrape it there.

## Endpoints

| Path | Purpose |
|---|---|
| `GET /metrics` | Prometheus exposition — the metrics below plus Go runtime and process metrics. |
| `GET /healthz` | Liveness — `200` while the process runs. |
| `GET /readyz` | Readiness — `200` once sabon is ready (after the first reconcile), else `503`. |

### Container healthcheck

The image ships a `HEALTHCHECK` that probes `/readyz` through the sabon binary
itself — the image is distroless, so there is no shell, `curl` or `wget` to
probe with:

```bash
docker compose exec sabon /sabon healthcheck              # -> ok
docker compose exec sabon /sabon healthcheck --endpoint healthz
```

It exits non-zero when the probe fails, so Docker marks the container
`unhealthy`. Mover containers reuse sabon's image but serve no endpoint, so
sabon disables the inherited healthcheck on them.

The endpoints do not depend on metrics being enabled: with a blank
`SABON_METRICS_ADDR`, sabon serves `/healthz` and `/readyz` on a separate
listener bound to `127.0.0.1:9333` — reachable for the container's own
healthcheck and nothing else. `SABON_HEALTH_ADDR` overrides that address (and
starts the listener even alongside the metrics server); blank on both switches
the endpoints off entirely, and the healthcheck then passes without probing.

## Metrics

### `sabon_build_info`

*gauge · labels `version`* — Constant `1`; exposes the build version.

### `sabon_ready`

*gauge* — `1` when the last reconcile succeeded, else `0`.

### `sabon_managed_jobs`

*gauge* — Number of discovered backup jobs currently managed.

### `sabon_reconciles_total`

*counter · labels `result`* — Reconcile runs, by `result` (`success`|`error`).

### `sabon_last_reconcile_timestamp_seconds`

*gauge* — Unix time of the last reconcile.

### `sabon_watch_restarts_total`

*counter* — Docker event-stream resubscriptions (self-heal).

### `sabon_deprecated_label_total`

*counter · labels `label`* — Uses of a deprecated label, by label name.

### `sabon_backups_total`

*counter · labels `app`, `target`, `result`* — Backup runs, by app, target and `result` (`success`|`error`).

### `sabon_backup_duration_seconds`

*histogram · labels `app`, `target`* — Backup run duration (`_bucket`/`_sum`/`_count`).

### `sabon_last_backup_success_timestamp_seconds`

*gauge · labels `app`, `target`* — Unix time of the last successful backup.

### `sabon_backup_files_new`

*gauge · labels `app`, `target`* — New files in the most recent backup.

### `sabon_backup_data_added_bytes`

*gauge · labels `app`, `target`* — Bytes added to the repo by the most recent backup.

### `sabon_checks_total`

*counter · labels `app`, `target`, `result`* — `restic check` runs by result (`success`/`error`).

### `sabon_last_check_success_timestamp_seconds`

*gauge · labels `app`, `target`* — Unix time of the last successful `restic check`.

### `sabon_prunes_total`

*counter · labels `app`, `target`, `result`* — `restic prune` runs by result (`success`/`error`).

### `sabon_last_prune_success_timestamp_seconds`

*gauge · labels `app`, `target`* — Unix time of the last successful `restic prune`.


The `sabon_backup_*` series are per `(app, target)`, mirroring the per-app,
per-target repository model — one time series per repository.

## Scraping

```yaml
scrape_configs:
  - job_name: sabon
    static_configs:
      - targets: ["127.0.0.1:9333"]
```

## Useful queries

```promql
# sabon has never reconciled successfully since start.
sabon_ready == 0

# A backup is failing for some app/target.
rate(sabon_backups_total{result="error"}[1h]) > 0

# Backups are stale — no successful backup for this app/target in over 24h.
time() - sabon_last_backup_success_timestamp_seconds > 86400

# Backup ratio succeeded vs total over the last day, per app/target.
sum by (app, target) (increase(sabon_backups_total{result="success"}[1d]))
  / sum by (app, target) (increase(sabon_backups_total[1d]))

# Backup duration p95, per app/target.
histogram_quantile(0.95, sum by (app, target, le) (rate(sabon_backup_duration_seconds_bucket[1d])))

# A deprecated label is still in use somewhere.
sabon_deprecated_label_total > 0

# Docker event watcher is flapping (falling back to periodic resync).
rate(sabon_watch_restarts_total[15m]) > 0
```

The staleness query is the important one: it catches the *silent* failure where a
schedule stops firing entirely, which a per-run alert on `..._total{result="error"}`
would miss. Alert on staleness, readiness, and a nonzero error rate.

## Dashboards & alerts

Ready-to-use monitoring lives in [`dashboards/`](https://github.com/davidborzek/sabon/tree/main/dashboards):

- [`grafana-dashboard.json`](https://github.com/davidborzek/sabon/blob/main/dashboards/grafana-dashboard.json) — import into
  Grafana and pick your Prometheus data source.
- [`prometheus-alerts.yaml`](https://github.com/davidborzek/sabon/blob/main/dashboards/prometheus-alerts.yaml) — alerting
  rules; reference it from `rule_files` in `prometheus.yml`.

## Notifications {#notifications}

Push notifications deliver each backup, check and prune outcome to one or more
services via [shoutrrr](https://containrrr.dev/shoutrrr/) (Slack, Telegram,
Discord, ntfy, generic webhook, …).

- `SABON_NOTIFY_URLS` — one or more shoutrrr URLs (comma-separated); each
  receives every notification. Empty disables it.
- `SABON_NOTIFY_ON` — `failure` (default; only failed backups) or `always`.

Title and body are rendered from Go `text/template`s. The built-ins read e.g.
`sabon: immich → onsite ok` / `Backup of immich to onsite succeeded in 1m30s
(snapshot ab12cd34, 5 new files, 3.0 MiB).` Override them with
`SABON_NOTIFY_TITLE_TEMPLATE` / `SABON_NOTIFY_TEMPLATE` — inline, or
`@/path/to.tmpl` to read from a file. The template context:

| Field | Notes |
| --- | --- |
| `.Event` | `backup`, `check` or `prune` |
| `.App` / `.Target` | app (repo) and target names |
| `.Instance` | `SABON_INSTANCE`, if set |
| `.OK` | outcome (bool) |
| `.Duration` | how long it took |
| `.SnapshotID` / `.FilesNew` / `.DataAdded` | backup success only |
| `.Error` | failure cause |

…plus the functions `short` (short snapshot id) and `bytes` (human size). A
custom template receives every event, so branch on `.Event` / `.OK` as needed —
e.g. Slack with a mention on failure:

```
{{if not .OK}}<!here> {{end}}{{.App}}/{{.Target}} {{if .OK}}✅{{else}}❌ {{.Error}}{{end}}
```
