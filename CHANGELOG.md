# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows
[Semantic Versioning](https://semver.org/).

## [0.3.0](https://github.com/davidborzek/sabon/compare/v0.2.0...v0.3.0) (2026-08-20)


### Features

* add healthcheck command ([#22](https://github.com/davidborzek/sabon/issues/22)) ([5cf4423](https://github.com/davidborzek/sabon/commit/5cf4423829693d44e7752050e6f996ba99d129d0))


### Bug Fixes

* **deps:** update module github.com/urfave/cli/v3 to v3.11.0 ([#20](https://github.com/davidborzek/sabon/issues/20)) ([db207e3](https://github.com/davidborzek/sabon/commit/db207e339cea4cf36ea337a8acbebf9569136f30))

## [0.2.0](https://github.com/davidborzek/sabon/compare/v0.1.0...v0.2.0) (2026-08-17)


### Features

* configurable custom labels on mover containers ([#17](https://github.com/davidborzek/sabon/issues/17)) ([7e967df](https://github.com/davidborzek/sabon/commit/7e967df8e0f13321b8d294969141444ebf323ec3))


### Bug Fixes

* **deps:** bump invopop/jsonschema and prometheus/client_golang ([3246853](https://github.com/davidborzek/sabon/commit/32468531413c5f851a56a0dfee5b528432785904))

## 0.1.0 (2026-07-28)

First public release of **sabon** — a label-driven [restic](https://restic.net)
backup orchestrator for Docker. Declare a backup next to each service and sabon
discovers it, backs it up on a schedule to its own repository, and restores it
with one command.

### Features

- **Label-driven discovery.** A container opts in with its own Compose labels
  (`sabon.enable` + a `sabon.backup` spec) — there is no central list of apps to
  maintain.
- **Per-app, per-target repositories.** Each `(app, target)` gets an isolated
  restic repository, so retention and restores stay independent and the blast
  radius is small. Back up to a local path and any restic-supported remote
  (S3, R2, WebDAV, …) — onsite, offsite, or both.
- **Ephemeral movers.** Every run spawns a throwaway container from sabon's own
  image (restic bundled), mounts the sources read-only, and runs `restic backup`
  then `restic forget`. Reclaiming space (`prune`) and integrity checks (`check`)
  run as separate, less frequent scheduled jobs.
- **Any volume.** Named volumes (any driver) and bind mounts are mounted by the
  mover, so nothing reaches into `/var/lib/docker`.
- **Consistency options.** Pre/post hooks in exec and one-shot run mode (e.g. a
  `pg_dump` before the backup), cold backups (`stop: true`), and atomic **ZFS
  snapshots** (`snapshot: zfs`, or `auto` to snapshot what's on ZFS and mount the
  rest live).
- **Restore & disaster recovery.** One-command restore into a staging directory
  or in place, path mapping, database-dump restore, and a bare-`restic` escape
  hatch for recovery when sabon itself is gone.
- **Docker Swarm support (experimental).** On a Swarm manager, sabon drives the
  cluster: it discovers services and runs each mover as a node-pinned one-shot
  service, with cold-backup quiescing. Validated by a single-node `docker swarm
  init` end-to-end test in CI; not yet validated against a multi-node cluster.
- **HTTP control API (optional, token-authenticated).** Trigger runs and browse
  durable run history — for dashboards and automation.
- **Observability.** Prometheus metrics, `/healthz` and `/readyz` endpoints,
  ready-made alert rules, Grafana dashboards, and failure notifications.
- **Timezone-aware scheduling.** Cron schedules and log timestamps honour `TZ`.

### Notes

- Pre-1.0 software: labels and configuration may change between minor releases.
- Distroless image with the `restic` binary bundled; movers reuse the same image,
  so a plain install pulls nothing else.
