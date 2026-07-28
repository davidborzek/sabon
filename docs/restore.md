---
tags:
  - Restore
  - Disaster recovery
---

# Restore

Restore is the point of all this, so the docs lead here. Every `(app, target)`
repository is **self-contained and encrypted**, so you can restore from any
machine that can reach the target — even after the original host and app are
gone. A restore is scoped to one `(app, target)` pair and a snapshot: find the
snapshot, then restore it into a **staging directory** (safe) or **in place**
(destructive).

!!! note "sabon runs as a container"
    sabon is a long-running container, so its CLI subcommands are executed
    **inside** it — e.g. `docker compose exec sabon sabon <subcommand>` (or
    `docker exec <sabon-container> sabon <subcommand>`). The examples below use
    the Compose form; drop the `docker compose exec sabon` prefix only if you run
    the `sabon` binary directly against the Docker socket.

## Listing snapshots

```sh
docker compose exec sabon sabon snapshots --app immich                  # all targets
docker compose exec sabon sabon snapshots --app immich --target offsite # one target
```

## Path mapping

Because the mover backed sources up from `/data/<name>`, that is how they appear
**inside the snapshot**. A source that was the volume `immich-library` is at
`/data/immich-library` in restic; the dump volume `immich-dbdump` is at
`/data/immich-dbdump`. Use these paths with `--include` to restore a subset.

## Staging restore — `--into` (recommended)

Restore into a host directory. This is non-destructive and works **even if the
app no longer exists** — the disaster-recovery path:

```sh
docker compose exec sabon sabon restore --app immich --target offsite \
  --snapshot latest \
  --into /srv/restore/immich
```

You then inspect the files under `/srv/restore/immich/data/<name>/…` and move
them into place yourself (e.g. recreate the volume, load `immich.sql` into a
fresh Postgres). `--snapshot` defaults to `latest`; pass a restic snapshot ID for
an earlier one. Narrow the restore with `--include`:

```sh
docker compose exec sabon sabon restore --app immich --target offsite \
  --into /srv/restore/immich \
  --include /data/immich-dbdump          # just the DB dump
```

!!! warning "The staging directory must be reachable by the mover"
    `--into` is a **host path on the Docker host** (the mover bind-mounts it).
    Point it somewhere the daemon can create, not a path that only exists inside
    the sabon container.

## In-place restore — `--in-place` (destructive)

Restore directly into the app's **live volumes**. This overwrites current data,
so stop the app first with `--stop` (sabon stops it, restores, and starts it
again):

```sh
docker compose exec sabon sabon restore --app immich --target offsite \
  --snapshot latest \
  --in-place --stop
```

`--into` and `--in-place` are mutually exclusive. Prefer `--into` unless you are
deliberately rolling a live app back and have accepted the data loss.

## Restoring a database

If you followed the dump pattern in [Hooks](hooks.md), the snapshot contains a
consistent `*.sql` (e.g. `/data/immich-dbdump/immich.sql`), **not** raw Postgres
files. Restore that file (staging is ideal), stand up a fresh database, and load
it:

```sh
sabon restore --app immich --target offsite \
  --into /srv/restore/immich --include /data/immich-dbdump
psql -h immich-postgres -U immich immich < /srv/restore/immich/data/immich-dbdump/immich.sql
```

The live `pgdata` was never in the repository, so there is nothing else to
restore for the database.

## Verify your backups are restorable

A backup you have never restored is a hope, not a backup. On a schedule — not
only during an incident:

- **Staging-restore** the latest snapshot into a scratch directory and confirm the
  files are complete and actually loadable (e.g. import the DB dump into a
  throwaway database).
- Run **`restic check`** on the repository. sabon can schedule this per target
  (see [check & prune](backups.md#checks)); run it out-of-band too before relying
  on a repo you have not touched in a while.

## Restore without sabon (bare restic)

The repositories are ordinary **restic** repositories — sabon is not required to
read them. This is the ultimate disaster-recovery escape hatch: with the repo
location and its password, plain `restic` restores everything.

A repository lives at `<target>/<app>`: a local target `path: /mnt/backup` puts
the `immich` repo at `/mnt/backup/immich`; a remote target uses the same
`<repo>/<app>` layout. Point `restic` straight at it:

```sh
export RESTIC_PASSWORD=…                  # the (app, target) repo password
restic -r /mnt/backup/immich snapshots
restic -r /mnt/backup/immich restore latest --target /srv/restore/immich
# remote, e.g.: restic -r s3:https://…/immich restore latest --target …
#   (with the backend's AWS_*/credentials in the environment)
```

Sources appear under `/data/<name>` inside the snapshot, exactly as with
`sabon restore`.
