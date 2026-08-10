---
tags:
  - Troubleshooting
  - Operations
---

# Troubleshooting

Almost every diagnosis starts the same way — validate the config and read the
orchestrator log:

```sh
docker compose exec sabon /sabon validate   # parse targets + every label, print what resolved
docker compose logs -f sabon               # discovery, schedules, mover lifecycle
```

`validate` never touches a repository; it only reports what sabon *would* do, so
it is safe to run any time.

## No movers spawn / nothing gets backed up

A container is only a job if it carries **both** `sabon.enable: "true"` **and** a
valid `sabon.backup` spec. In **swarm mode** the labels must be on the service
(`deploy.labels`), not on the container. Check that `sabon validate` lists the app,
then look for exited movers:

```sh
docker ps -a  --filter label=sabon.mover        # standalone
docker service ls --filter label=sabon.mover    # swarm
```

Each finished mover is retained as run history (see
[`SABON_MOVER_HISTORY`](configuration.md#sabon_mover_history)); its restic output
is in `docker logs <id>` / `docker service logs <id>`.

## `403` / permission denied against the Docker API

You are almost certainly behind a socket proxy that is missing an object group.
sabon needs `POST=1` **plus**:

- standalone — `CONTAINERS`, `IMAGES`, `VOLUMES`, `EXEC`, `EVENTS`
- swarm — `SERVICES`, `TASKS`, `NODES`, `IMAGES` (point it at a **manager**)

See [socket access](deployment.md#socket) for a complete proxy example.

## `repository is already locked`

A previous restic run was killed before it released its lock. Confirm no mover is
still running for that repo, then clear it:

```sh
restic -r <repo> unlock
```

A `prune` job locks the repo for its whole duration and blocks backups — that is
expected; see [check & prune](backups.md#checks).

## `wrong password` / repository not found

A restic repository's password is fixed when it is first created. If
`RESTIC_PASSWORD` (or a per-target password) later differs, restic cannot open the
repo. Restore the original password, or start a new repository. The repo location
is `<target>/<app>` (e.g. `/mnt/backup/immich`).

## `snapshot: zfs` failed the backup

`snapshot: zfs` is strict: the source must actually live on a ZFS dataset, and the
snapshotter only runs on **standalone Docker**. It fails (rather than silently
copying live data) when the source is not on ZFS, and it is **unsupported in
swarm**. Use `snapshot: auto` (snapshot what is on ZFS, mount the rest live) or
`none`. See [Snapshots](backups.md#snapshots).

## Swarm: the mover never starts / image pull fails

Movers run as services scheduled onto arbitrary nodes, so the mover image must be
pullable on **every** node. Self-detection is unreliable for a Swarm task — pin it
explicitly:

```sh
SABON_MOVER_IMAGE=ghcr.io/davidborzek/sabon:latest
```

Also check the mover isn't stuck `Pending`: a `node.hostname` placement constraint
with no matching node (e.g. the app's node left the cluster) leaves it unscheduled.

## A local target only backs up on one node (swarm)

A local `path:` target is a host directory on whichever node the mover lands on —
it is meaningless cluster-wide. On a multi-node cluster use a **remote** target
(S3/R2/…) so every node writes to the same repository.

## Restores look empty

Sources are stored under `/data/<name>` inside the snapshot, so an `--include`
must use that prefix (`--include /data/immich-dbdump`, not `/etc`). See
[Path mapping](restore.md#path-mapping).
