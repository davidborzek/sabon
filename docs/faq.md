# FAQ

## Why do the movers run as root?

A backup tool has to read **every** app's data, and that data is owned by many
different uids. Running the mover as root is the only way to read all of it without
per-app chowns. The **orchestrator** itself runs nonroot — only the short-lived
movers are privileged, and only for the duration of a backup. Narrow it with
`SABON_MOVER_USER` (or the per-app `user` label / `groups`) when every source and
repo is readable by one uid. See [Privileges](deployment.md#privileges).

## Why a separate repository per app?

Isolation. Each `(app, target)` gets its own restic repository, so retention and
restores are independent and the blast radius of a damaged repo is one app, not
everything. It also keeps restores scoped and fast.

## Does sabon run `restic prune`?

Not on every backup. Each run calls `restic forget` to **mark** snapshots for
removal (cheap, no lock). Reclaiming space with `restic prune` **locks the
repository** and blocks backups, so it runs as a separate, less frequent scheduled
job. See [check & prune](backups.md#checks).

## Can I restore without sabon?

Yes. The repositories are ordinary restic repositories — with the repo location
and its password, plain `restic` restores everything, no sabon required. See
[Restore without sabon](restore.md#restore-without-sabon-bare-restic). This is the
point of the design: sabon orchestrates, but it is never a dependency for recovery.

## Where do backend credentials go?

In the orchestrator's environment (e.g. `RESTIC_PASSWORD`, `AWS_ACCESS_KEY_ID`),
referenced from the targets file. sabon passes them through to the movers. Keep
them out of version control and image layers — inject them from a secret store at
runtime. See the [threat model](https://github.com/davidborzek/sabon/blob/main/SECURITY.md).

## Does it back up named volumes, or only bind mounts?

Both. Because a mover *mounts* the source, sabon backs up named volumes of any
driver as well as bind mounts, without reaching into `/var/lib/docker`.

## How do I get a consistent database backup?

Don't back up live Postgres/MySQL data files. Write a dump in a pre-hook and back
up the dump instead (or quiesce the app with `stop: true`, or use a ZFS snapshot).
See [Hooks](hooks.md) for the `pg_dump` pattern.

## Is Docker Swarm production-ready?

No — Swarm support is **experimental**. It is validated only by a single-node
`docker swarm init` end-to-end test in CI and has not been tested against a
multi-node production cluster. ZFS snapshots and exec-mode hooks are unsupported in
swarm. See [Deployment](deployment.md#swarm).

## Which restic version does sabon use?

Whichever is bundled in the sabon image for that release — a single version shared
by the orchestrator and every mover, since movers reuse the sabon image. Pin the
sabon image tag to pin restic.
