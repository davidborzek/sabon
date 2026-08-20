---
tags:
  - Configuration
  - Reference
---

# Configuration

sabon has two configuration surfaces:

1. **Process configuration** — environment variables (`SABON_*`) that control the
   orchestrator itself.
2. **The targets file** — a YAML document describing the backup backends
   (restic repositories) and their schedules and retention.

Per-container backup policy lives in **labels**, documented separately in
[Labels](labels.md).

## Environment variables

### `SABON_RUNTIME`

*default `auto`* — Which container runtime to drive: `auto` detects it (a Docker **Swarm manager** → swarm, anything else → standalone), or force `standalone` / `swarm`. See [Docker Swarm](#docker-swarm).

### `SABON_LABEL_PREFIX`

*default `sabon`* — Label namespace (`<prefix>.enable`, `<prefix>.backup`, …).

### `SABON_WATCH_BY_DEFAULT`

*default `false`* — When `true`, back up every container unless it sets `<prefix>.enable=false`. When `false` (default), only containers with `<prefix>.enable=true`.

### `SABON_CONFIG`

*default `/etc/sabon/targets.yaml`* — Path to the targets file.

### `SABON_RESYNC_INTERVAL`

*default `5m`* — Periodic full reconcile, on top of event-driven ones.

### `SABON_DEBOUNCE_DELAY`

*default `2s`* — Coalesces bursts of container events into one reconcile.

### `SABON_METRICS_ADDR`

*default `:9333`* — Listen address for `/metrics`, `/healthz`, `/readyz`. Blank disables the server; the health endpoints then move to `SABON_HEALTH_ADDR`.

### `SABON_HEALTH_ADDR`

*default: `127.0.0.1:9333` when metrics are disabled, else unused* — Separate listener for `/healthz` and `/readyz`, so the [container healthcheck](metrics.md#container-healthcheck) keeps working without the metrics server. Set it to run the listener alongside metrics too; blank disables it.

### `SABON_LOG_LEVEL`

*default `info`* — `debug`, `info`, `warn`, `error`. `debug` traces what the daemon does: discovered jobs and the container events that trigger reconciles, scheduler (re/un)registration, and the per-run mechanics — resolved snapshot mode, each host `zfs` call, snapshot create/destroy, and every mover spawn.

### `SABON_MOVER_IMAGE`

*default: auto-detect* — Image used for mover containers. Empty reuses sabon's own running image — auto-detected inside Docker (the exact ref you deployed: registry, digest, fork and all), else the version-matched `ghcr.io/davidborzek/sabon:<version>`. Set it explicitly for a custom mover image, or on a dev build with no version stamped in.

### `SABON_MOVER_USER`

*default `0:0`* — `uid:gid` movers run as. Root by default — movers must read arbitrary app data and write repos. Labels may override per app (`user`).

### `SABON_MOVER_GROUPS`

*default: none* — Comma-separated supplementary groups (GIDs) added to every mover (`--group-add`), so a nonroot mover can read group-owned data without a chown. Labels may override per app (`groups`).

### `SABON_MOVER_NETWORK`

*default: none* — Optional Docker network to attach movers to (e.g. to reach a remote backend by service name).

### `SABON_MOVER_LABELS`

*default: none* — Comma-separated `key=value` pairs set as labels on every mover container (e.g. `SABON_MOVER_LABELS=team=infra,cost-center=backups`) — useful for external log/metrics scrapers or housekeeping tools. Keys must not use the reserved `sabon.` prefix; sabon's own run-history labels (`sabon.app`/`sabon.target`/`sabon.action`) always win.

### `SABON_CACHE_VOLUME`

*default `sabon-cache`* — Named volume mounted at `/cache` in movers as the shared restic cache.

### `SABON_MOVER_HISTORY`

*default `3`* — Exited mover containers kept per app/target/action as run history — each run's status and restic logs, readable via the [HTTP API](api.md#run-history) or directly with `docker ps -a` / `docker logs` (in swarm mode these are retained mover *services*, seen with `docker service ls` / `docker service logs`), and surviving restarts. `0` removes every exited mover (no history).

### `SABON_RUN_ON_STARTUP`

*default `false`* — When `true`, run all backups once immediately on startup, then follow the schedules.

### `SABON_INSTANCE`

*default: none* — When set, only manage containers whose `<prefix>.instance` matches — for running several sabon instances side by side. Empty = manage all.

### `SABON_MAX_PARALLEL`

*default `0`* — Cap on concurrent backups (movers). `0` = unlimited. Prevents a thundering herd when many apps share a schedule.

### `SABON_SCHEDULE_JITTER`

*default `0`* — Random `0..jitter` delay before each *scheduled* run, to spread load off exact cron boundaries.

### `SABON_BACKUP_TIMEOUT`

*default `0`* — Deadline per backup run; `0` = none. A run that exceeds it is aborted and its mover removed. Labels may override per app (`timeout`).

### `SABON_SNAPSHOT`

*default `none`* — Default source-snapshot strategy: `none` (mount live), `zfs` (snapshot each source's dataset first; a source not on ZFS fails the backup), or `auto` (snapshot the ZFS sources, mount the rest live). Labels may override per app (`snapshot`). See [Snapshots](backups.md#snapshots).

### `SABON_SNAPSHOT_ZFS_IMAGE`

*default `ghcr.io/davidborzek/sabon/zfs-snapshotter:<sabon version>`* — Privileged image the **zfs** provider uses to run the host's `zfs`. A released binary defaults to the snapshotter tagged with its own version; an unversioned dev build has **no** default and must set this to use ZFS snapshots (it will not silently pull a floating `:latest` privileged image). Override to pin or replace it. See [Snapshots](backups.md#snapshots).

### `SABON_NOTIFY_URLS`

*default: none* — Comma-separated [shoutrrr](https://containrrr.dev/shoutrrr/) URLs (Slack, Telegram, Discord, ntfy, generic webhook, …); every one receives each notification. Empty disables notifications.

### `SABON_NOTIFY_ON`

*default `failure`* — When to notify: `failure` (only failed backups) or `always`.

### `SABON_NOTIFY_TITLE_TEMPLATE`

*default: built-in* — Go [text/template](https://pkg.go.dev/text/template) for the notification **title**, overriding the built-in. A value beginning with `@` is read from that file. See [Notifications](metrics.md#notifications) for the template data and functions.

### `SABON_NOTIFY_TEMPLATE`

*default: built-in* — Go text/template for the notification **body** (same `@file` support and data model as the title template).

### `SABON_API_ADDR`

*default: none* — Listen address for the [HTTP control API](api.md) (e.g. `:8080`); empty disables it. It is privileged (a restore overwrites live data) — bind it to loopback and put TLS in front.

### `SABON_API_TOKEN`

*default: none* — Bearer token for the API. When set, every request must carry it (`Authorization: Bearer <token>`). **Empty disables authentication** — every request is allowed and sabon logs a warning at startup; only acceptable for a loopback bind behind a trusted proxy. Recommended to set.

### `SABON_HOOK_ENV_<NAME>`

*default: none* — Fallback allowlist for hook environment interpolation. A `${NAME}` reference in a hook's `env:` value (in a container label) resolves **first** from the labelled container's own environment, and falls back to **`SABON_HOOK_ENV_<NAME>`** from sabon's environment for a value that lives nowhere on the app's container (e.g. only in a sibling database). A label can never read sabon's `RESTIC_PASSWORD`, cloud credentials or another target's password. Prefer exec-mode hooks, which need no injection at all.

Expose such a value on the sabon service, and reference it in the label escaped for Compose (`$$`):

```yaml
services:
  sabon:
    environment:
      SABON_HOOK_ENV_DB_PASSWORD: ${DB_PASSWORD}   # sabon side: ordinary Compose interpolation
# in the sabon.backup label:   env: { PGPASSWORD: "$${DB_PASSWORD}" }
```

This applies only to **hook** `env`; the `env:` block in the targets file is operator-owned and expands from the full environment. See [Hooks](hooks.md#credentials-in-hooks).

### `DOCKER_HOST`

*default: docker default* — Standard Docker client variable; point it at a socket proxy.

### `TZ`

*default `UTC`* — Standard timezone variable (e.g. `Europe/Berlin`). Cron schedules and log timestamps are interpreted in it; unset means UTC. The tz database is embedded in the image, so no `/etc/localtime` mount is needed. An invalid value falls back to UTC. restic records snapshot times in **UTC** (the mover has no zoneinfo) — deliberately: UTC is unambiguous and DST-safe.


### restic credentials

Movers need the restic repository password and any backend credentials. These
are passed through from the orchestrator's environment:

- **`RESTIC_PASSWORD`** — the repository password, unless a target overrides the
  variable name with `passwordEnv`.
- **Backend credentials** — e.g. `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`,
  referenced from a target's `env:` block (see below).

Keep these in a `.env` file, a Docker secret, or your secret manager — never in
the labels.

## Docker Swarm

With `SABON_RUNTIME=swarm` (or auto-detected on a manager node) sabon drives the Swarm **service** API instead of local containers. sabon must run **on a manager node**. What changes:

- **Discovery** reads **services**, not containers. Put the backup labels in the service's `deploy.labels` (not the container labels): `sabon.enable`, `sabon.backup`, … Sources come from the service's mounts plus the spec's `extraVolumes`/`extraPaths`.
- **Placement** — each job is pinned to the node its task currently runs on, and the mover runs there as a one-shot service (a `node.hostname` constraint) so it reaches the node-local volumes. A **scaled-to-zero** service has no running task, so its node is taken from its `node.hostname`/`node.id`/`node.labels` placement constraint instead; only a service with neither a running task nor a pinning constraint is left unconstrained (may land anywhere).
- **Cold backups** scale the service to **zero replicas** and back. The original count is recorded on the service (`sabon.quiesce.replicas` label), not in memory, so a crash between stop and start cannot strand it at zero — sabon restores any stranded service on startup.
- **Hooks** — run-mode hooks run as one-shot services; **exec-mode hooks are not supported** (a manager cannot exec into a task on another node) and return an error. Env and compose project are read from the service spec.

Not supported in swarm mode: **ZFS snapshots** (`SABON_SNAPSHOT=zfs`/`auto` — the snapshotter is node-local) and cross-node exec hooks. Named volumes are node-local unless backed by a cluster volume driver.

> Swarm mode is exercised by a single-node `docker swarm init` end-to-end test in CI; it has not been validated against a multi-node production cluster.

## The targets file

The targets file (`SABON_CONFIG`, default `/etc/sabon/targets.yaml`) lists the
backends. Every target becomes a per-app repository: an app named `paperless`
backed up to a target named `onsite` lives in that target's repo for `paperless`.

```yaml
targets:
  - name: onsite
    path: /mnt/backup             # local backend: repo = /mnt/backup/<app>
    schedule: "0 0 */6 * * *"     # 6-field cron (seconds first)
    retention: { hourly: 24, daily: 7, weekly: 5 }

  - name: offsite
    repo: "s3:${R2_ENDPOINT}/${R2_BUCKET}/{app}"   # remote backend
    passwordEnv: RESTIC_PASSWORD  # default; name of the env var holding the password
    env:
      AWS_ACCESS_KEY_ID: ${R2_ACCESS_KEY_ID}
      AWS_SECRET_ACCESS_KEY: ${R2_SECRET_ACCESS_KEY}
    schedule: "0 30 3 * * *"
    retention: { daily: 7 }
```

### Target fields

#### `name`

Target name. Referenced from a label's `targets`, `schedule` and `retention` maps.

#### `path`

**Local backend.** The repo for app `<app>` is `<path>/<app>`, bind-mounted into the mover at `/repo`.

#### `repo`

**Remote backend.** A restic repository string with `{app}` and `${ENV}` placeholders, e.g. `s3:…/{app}`. See restic's [repository backends](https://restic.readthedocs.io/en/stable/030_preparing_a_new_repo.html) for the supported schemes (`s3:`, `rest:`, `sftp:`, `b2:`, …).

#### `passwordEnv`

Name of the environment variable holding the repo password. Default `RESTIC_PASSWORD`.

#### `passwordFile`

Host path to a file holding the repo password. Mounted **read-only** into the mover; restic reads it via `RESTIC_PASSWORD_FILE`, so the password is in **no** container's environment. Overrides `passwordEnv`.

#### `credentialsFile`

Host path to an AWS shared-credentials file (`[default]` with `aws_access_key_id`/`aws_secret_access_key`). Mounted **read-only** and exposed via `AWS_SHARED_CREDENTIALS_FILE` so S3/B2/… creds stay out of the mover's environment.

#### `env`

Extra environment (map) passed to movers for this target — backend credentials, endpoints, etc. Values support `${ENV}` expansion.

#### `resticArgs`

Extra **global** restic flags prepended to every invocation for this target (e.g. `--limit-upload`, `--pack-size`, `--compression`, `-o key=value`). See restic's [backup options and tuning parameters](https://restic.readthedocs.io/en/stable/040_backup.html).

#### `moverLabels`

Extra labels (map) set on every mover container spawned for this target, merged on top of the global [`SABON_MOVER_LABELS`](#sabon_mover_labels) (target wins on key collisions). Keys must not use the reserved `sabon.` prefix; sabon's own run-history labels always win.

#### `schedule`

6-field cron (seconds first) for this target. Labels may override it per app.

#### `retention`

restic retention policy for this target — see [restic's forget policies](https://restic.readthedocs.io/en/stable/060_forget.html). Labels may override it per app.

#### `check`

Optional 6-field cron for periodic `restic check` ([repository integrity](https://restic.readthedocs.io/en/stable/045_working_with_repos.html)) of this target's per-app repos. Empty disables it. See [check & prune](backups.md#checks).

#### `prune`

Optional 6-field cron for periodic `restic prune` (repack / reclaim space; see [restic forget & prune](https://restic.readthedocs.io/en/stable/060_forget.html)). Empty disables it — backups then only run `forget` (mark), and space is reclaimed by this job. See [check & prune](backups.md#checks).


**Exactly one of `path` or `repo`** must be set per target — a target is either
local or remote.

Prefer `passwordFile` over `passwordEnv` to keep the repo password out of every
container's environment (it is otherwise visible via `docker inspect`). Point it
at a Docker secret or a `0600` file; it must be readable by the mover user
(`SABON_MOVER_USER` / label `user`). For remote backends, `credentialsFile`
does the same for S3/B2/… credentials: an AWS shared-credentials file mounted
read-only and exposed via `AWS_SHARED_CREDENTIALS_FILE`, keeping the keys out of
`env` too. Don't also put `AWS_*` in `env` — env wins over the file.

### Placeholders

- **`{app}`** — expands to the app name (from the label's `repo`, else the
  compose service, else the container name). Used to give every app its own repo
  inside a shared remote bucket.
- **`${ENV}`** — expands from the orchestrator's environment at load time, in
  both `repo` and `env` values. Keeps endpoints and secrets out of the file.

### Retention keys

`retention` maps directly to restic's `forget --keep-*` policy. Any combination
of these keys is allowed:

| Key | Keeps |
|---|---|
| `last` | The N most recent snapshots. |
| `hourly` | The last snapshot for each of N hours. |
| `daily` | The last snapshot for each of N days. |
| `weekly` | The last snapshot for each of N weeks. |
| `monthly` | The last snapshot for each of N months. |
| `yearly` | The last snapshot for each of N years. |
| `within` | All snapshots within a duration, e.g. `within: 14d`. |

Retention marks old snapshots after each successful backup (`restic forget`);
repacking to reclaim space is a separate `prune` job — see [Backups](backups.md).

### Cron format

Schedules are **6-field** cron expressions with **seconds first**
(`sec min hour dom mon dow`). Examples:

| Expression | Meaning |
|---|---|
| `0 0 */6 * * *` | Every 6 hours, on the hour. |
| `0 30 3 * * *` | Daily at 03:30. |
| `0 0 2 * * 0` | Weekly, Sunday 02:00. |

Schedules run in the [`TZ`](#tz) timezone (UTC if unset).

Validate a config without running anything with `sabon validate` (see
[Labels](labels.md)).

### Schema & editor validation {#schema}

The targets file has a JSON schema —
[`schemas/targets.json`](https://github.com/davidborzek/sabon/blob/main/schemas/targets.json)
(regenerate with `sabon schema --targets`). Point your editor at it via
[yaml-language-server](https://github.com/redhat-developer/yaml-language-server)
for completion and validation of your `targets.yaml`:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/davidborzek/sabon/main/schemas/targets.json
targets:
  - name: onsite
    path: /mnt/backup
```
