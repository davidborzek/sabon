# Restore

Restore is scoped to one `(app, target)` pair and a snapshot. First find the
snapshot, then restore it either into a **staging directory** (safe) or
**in place** (destructive).

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
