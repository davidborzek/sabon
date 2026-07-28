---
tags:
  - Backups
  - Snapshots
  - Retention
---

# Backups

This is the domain guide: what actually happens when sabon backs an app up, how
retention works, and how you find out when something breaks. Getting your data back has its own
page: [Restore](restore.md).

## How a backup runs

When a job's schedule fires (or you run `sabon backup`), the orchestrator spawns
one **mover** container per `(app, target)` pair and the mover does the work:

1. **Mount** — every source is mounted read-only at `/data/<name>`, the local
   repo (if any) as a bind at `/repo`, and the shared restic cache at `/cache`.
2. **`restic unlock`** — clears any stale lock left by a previously killed mover.
3. **preHooks** — e.g. a database dump into an included volume (see
   [Hooks](hooks.md)).
4. **`restic backup`** — snapshots the `/data/*` sources into the app's
   per-target repository, applying the label's `exclude` patterns and `tags`.
5. **Retention** — `restic forget` applies the target's (or the label's
   per-target override) retention policy, *marking* old snapshots. Repacking to
   reclaim space is a separate `prune` job (see below), so backups stay fast.
6. **postHooks** — any cleanup / success hooks.
7. The mover exits; the orchestrator reaps it and records metrics.

Runs against the **same repository** are serialized (per-repo lock), so retention
and prune never collide with a concurrent backup. Different apps back up in
parallel. If sabon crashes mid-backup the mover keeps running and finishes on its
own — see [Architecture](architecture.md#lifecycle-and-robustness).

### Repositories

Each app gets its **own repository per target**:

- Local target `path: /mnt/backup` → `/mnt/backup/<app>`.
- Remote target `repo: "s3:…/{app}"` → `{app}` expands to the app name.

Snapshots, retention, listing and restore are always scoped to one
`(app, target)` pair.

### Running on demand

```sh
sabon backup                 # run every app's backups once, then exit
sabon backup --app immich    # just this app (all its targets)
sabon validate               # print the plan without backing up
```

Set `SABON_RUN_ON_STARTUP=true` to run everything once at daemon startup before
following the schedules.

## Volumes & storage drivers {#drivers}

sabon backs a named volume up by **mounting it into the mover by name** — the
same way any container mounts it — so Docker and the volume's driver do the
work. sabon never reads `/var/lib/docker/volumes` or assumes a host path, so
**backups work with any volume driver** (the built-in `local`, `local` with
NFS/CIFS options, or third-party plugins like Portworx, REX-Ray, GlusterFS, …)
just as they do with bind mounts.

Two things are worth knowing:

- **The mover must be able to mount the volume at backup time.** Share-style
  drivers (`local`, NFS, CIFS, GlusterFS) mount fine read-only *alongside* the
  running app. A few **single-attach block drivers** attach a volume to only
  one container at a time; there the mover's concurrent read-only mount can fail
  while the app still holds the volume. Set **`stop: true`** on the app so sabon
  cold-backs it up (stops it for the run, restarts it after) and the mover can
  attach — see [Consistency](#consistency).
  `sabon validate` prints each volume's driver and flags non-`local` ones with
  exactly this hint, so you can spot the risk before a run.

- **`snapshot: zfs` needs the source to *really* live on a ZFS dataset.** The
  strategy resolves each source to its host location (a bind path, or a volume's
  `Mountpoint` via `docker volume inspect`) and snapshots the ZFS **dataset**
  that holds it. A volume only qualifies when its data is genuinely on local ZFS
  storage: the **`local`** driver *without* mount options. sabon rejects a
  non-`local` (plugin) volume, or a `local` volume with driver options (an
  NFS/CIFS/tmpfs/… mount overlaid on its mountpoint) — a ZFS snapshot of that
  mountpoint would capture the empty directory *under* the overlay, not the
  data. Such sources (and anything not on ZFS) show up in `sabon validate` as
  *not snapshottable* with the reason, and **fail the backup** under
  `snapshot: zfs` rather than silently copying nothing (`snapshot: auto` mounts
  them live instead). Use a preHook dump or `stop: true` for them. See
  [Filesystem snapshots](#snapshots).

!!! note "Bind paths that are themselves foreign mounts"
    The overlay check uses Docker's volume metadata, so it covers named volumes.
    A **bind** source that is itself a non-ZFS mount (e.g. an NFS export mounted
    at a path under a ZFS dataset) can still be misresolved — sabon has no volume
    metadata for raw paths. Point `snapshot: zfs` only at binds that are truly on
    ZFS.

## Consistency {#consistency}

restic copies files as it walks them, so backing up data that is written
*during* the run can be internally inconsistent. sabon gives you three portable
ways to get a consistent copy — choose per app:

- **Databases → dump in a preHook.** Run `pg_dump` / `mysqldump` /
  `sqlite3 … ".backup"` into a path that is part of the backup set, and exclude
  the live data directory. This yields a logical, point-in-time-consistent dump
  on any filesystem. See [Hooks](hooks.md).
- **Whole-app quiesce → `stop: true`.** sabon stops the container for the
  duration of the backup (cold backup) and restarts it afterwards — simple and
  filesystem-agnostic, at the cost of a short downtime.
- **Live files → usually fine.** Media and config files are individually
  consistent; restic copes with concurrent changes.
- **Atomic snapshots → `snapshot: zfs` (or `auto`).** For large, un-stoppable
  stores on ZFS, back up from a `zfs snapshot` of the source's dataset instead of
  the live files — crash-consistent, zero downtime. `zfs` requires every source
  to be on ZFS; `auto` snapshots the ZFS sources and mounts the rest live. See
  [below](#snapshots).

### Filesystem snapshots {#snapshots}

Set `snapshot: zfs` (globally via `SABON_SNAPSHOT`, or per app via the label) to
back up from an atomic, read-only **ZFS snapshot** instead of the live path. For
each source sabon resolves the host path (a bind path, or a named volume's
mountpoint), finds the ZFS **dataset** that holds it, takes a `zfs snapshot`,
and mounts that snapshot read-only into the
mover. The snapshot is destroyed after the run.

**Granularity is per dataset, not per folder.** If `/mnt/appdata` is one dataset
and `immich` is just a directory in it, sabon snapshots the *whole* `appdata`
dataset (ZFS has no per-directory snapshots) and reads only the `immich`
subpath — the backup still contains only immich, and the snapshot is cheap
(copy-on-write, instant, released immediately). For a per-app snapshot, make
each app its own child dataset (`appdata/immich`): sabon always snapshots the
*longest-matching* dataset, so it adapts to your layout automatically.

**Requirements.** ZFS snapshots are host-privileged, so the `zfs` calls run in a
small, ephemeral **privileged** image (`SABON_SNAPSHOT_ZFS_IMAGE`) that runs the
host's own `zfs`. The source
must actually be on ZFS — `snapshot: zfs` on a non-ZFS source **fails the
backup** rather than silently copying live data. Which volumes qualify depends
on the driver — see [Volumes & storage drivers](#drivers). Use `snapshot: auto`
to relax this: it snapshots the sources that are on ZFS and mounts the rest
(non-ZFS or foreign-backed volumes) **live** in the same run — atomic where it
can, live where it can't.

!!! warning "ZFS snapshots need standalone Docker"
    The privileged ZFS snapshotter is a node-local container that can't run as a
    node-pinned Swarm service, so `snapshot: zfs`/`auto` are unsupported on Swarm
    — sources mount **live** there. See [Deployment](deployment.md#swarm).

If a run crashes mid-snapshot, the orphan is cleaned up at the next startup
(when a snapshotting mode is configured — `zfs` or `auto`); a snapshot still read
by a mover that survived a restart is spared until that backup finishes.

## Maintenance: check & prune {#checks}

Two optional, scheduled maintenance jobs run against a target's per-app
repositories, each on its own cron.

**`check`** — verify repository integrity with
[`restic check`](https://restic.readthedocs.io/en/stable/077_troubleshooting.html).

**`prune`** — reclaim space with `restic prune`. Backups themselves only run
`forget` (marking old snapshots per the retention policy); the actual repack is
deferred to this less-frequent job, so backups stay fast and prune's heavy I/O
happens on your schedule.

```yaml
targets:
  - name: onsite
    path: /mnt/backup
    schedule: "0 0 */6 * * *"   # backup every 6h (forget marks old snapshots)
    check: "0 0 5 * * 0"        # verify weekly, Sundays 05:00
    prune: "0 30 5 * * 0"       # reclaim space weekly, Sundays 05:30
```

Outcomes are recorded as `sabon_checks_total` / `sabon_prunes_total` (with
`_last_*_success_timestamp_seconds`), and a failure triggers a notification (if
configured). Run either on demand:

```sh
docker compose exec sabon sabon check --app immich --target onsite
docker compose exec sabon sabon prune --app immich --target onsite
```
