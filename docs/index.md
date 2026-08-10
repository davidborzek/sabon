# sabon

**Label-driven restic backups for Docker** — declare a backup in the service's
own Compose labels, and sabon discovers it and backs it up with
[restic](https://restic.net) on a schedule. Per-app, per-target repositories you
restore from with one command — including full disaster recovery when the app is
gone.

[Get started](#quick-start){ .md-button .md-button--primary }
[View on GitHub](https://github.com/davidborzek/sabon){ .md-button }

<div class="grid cards" markdown>

-   🏷️ **Declared next to the service**

    ---

    The backup spec lives in the app's own Compose labels — not a central,
    disconnected config.

    [Labels →](labels.md)

-   📦 **Per-app, per-target repos**

    ---

    Each app gets isolated restic repositories, so retention and restores stay
    independent and the blast radius stays small.

    [Backups →](backups.md)

-   🔐 **Incremental & encrypted**

    ---

    Movers run **restic**: each run stores only changed data, deduplicated and
    client-side encrypted; a scheduled `restic check` verifies repository integrity.

    [Backups →](backups.md)

-   🗃️ **Any volume driver**

    ---

    Movers mount volumes by name, so named volumes (any driver) and bind mounts
    back up alike.

    [Storage drivers →](backups.md#drivers)

-   🐝 **Standalone or Swarm**

    ---

    Drives a single Docker host by default; on a Swarm manager it drives the
    whole cluster, running each mover as a node-pinned service.

    [Deployment →](deployment.md#swarm)

-   📸 **Atomic or best-effort snapshots**

    ---

    `snapshot: zfs` backs up from a crash-consistent ZFS snapshot — no app
    downtime; `auto` snapshots what it can and mounts the rest live.

    [Snapshots →](backups.md#snapshots)

-   ♻️ **Restore anywhere**

    ---

    One command restores a snapshot into a staging directory or in place — even
    if the original app is gone.

    [Restore →](restore.md)

-   📊 **Metrics & alerts**

    ---

    Prometheus metrics, health endpoints, ready-made alert rules, and
    notifications catch silent failures.

    [Metrics →](metrics.md)

-   🔌 **HTTP control API**

    ---

    An optional, token-authenticated API to trigger runs and browse durable
    run history — for dashboards and automation.

    [HTTP API →](api.md)

</div>

!!! info "Pre-1.0"
    Early-stage software — expect occasional breaking changes to labels or
    configuration before 1.0. Pin an image tag and skim the release notes when
    upgrading.

## Quick start

Label a container and run sabon alongside it:

```yaml
services:
  paperless:
    image: ghcr.io/paperless-ngx/paperless-ngx
    volumes:
      - paperless-data:/usr/src/paperless/data
    labels:
      sabon.enable: "true"
      sabon.backup: |
        repo: paperless
        auto: true            # back up this container's own volumes

  sabon:
    image: ghcr.io/davidborzek/sabon:latest
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ./targets.yaml:/etc/sabon/targets.yaml:ro
      - /mnt/backup:/mnt/backup
    environment:
      RESTIC_PASSWORD: change-me
    restart: unless-stopped

volumes:
  paperless-data:
```

With a `targets.yaml` that points at where the repos live:

```yaml
targets:
  - name: onsite
    path: /mnt/backup             # repo = /mnt/backup/paperless
    schedule: "0 0 */6 * * *"     # every 6 hours (6-field cron)
    retention: { daily: 7, weekly: 5 }
```

sabon discovers the `paperless` container, and every six hours a mover container
runs `restic backup` of `paperless-data` into `/mnt/backup/paperless`, then
applies retention. Run one immediately without waiting for the schedule:

```sh
docker compose exec sabon /sabon backup --app paperless
```

!!! tip "Running Docker Swarm?"
    sabon can drive a whole cluster too, running each mover as a node-pinned
    service — see [Docker Swarm](deployment.md#swarm) (experimental).

## Documentation

- [Configuration](configuration.md) — every `SABON_*` env var and the
  targets-file schema.
- [Labels](labels.md) — the `sabon.backup` document form and worked examples.
- [Hooks](hooks.md) — pre/post hooks in exec and run mode, incl. `pg_dump`
  patterns for consistent database backups.
- [Backups](backups.md) — how backups work, consistency/snapshots, notifications.
- [Restore](restore.md) — staging vs in-place, path mapping, database restore.
- [Deployment](deployment.md) — Compose, plain Docker, socket access, and the
  root/Docker-write privilege model.
- [Observability](metrics.md) — metrics, health endpoints, PromQL, dashboards, notifications.
- [Architecture](architecture.md) — how it works under the hood: the
  orchestrator + ephemeral mover model, discovery, per-repo serialization, and
  crash/shutdown behaviour.
- [HTTP API](api.md) — the optional, token-authenticated control API for
  triggering runs and browsing durable run history.
- [Troubleshooting](troubleshooting.md) — first-response fixes for the common failures.
- [FAQ](faq.md) — why root movers, per-app repos, `prune`, bare-restic restore, Swarm.

## Images

`ghcr.io/davidborzek/sabon` — a distroless image with the restic binary bundled.
Movers reuse this same image, so a plain install pulls nothing else.

`ghcr.io/davidborzek/sabon/zfs-snapshotter` — a small privileged image the `zfs`
and `auto` snapshot strategies pull to run the host's `zfs` (overridable via
`SABON_SNAPSHOT_ZFS_IMAGE`). Pulled only when snapshotting is enabled.
