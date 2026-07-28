# Example

A complete, runnable sabon stack: the orchestrator, a least-privilege Docker
socket proxy, and a small labelled workload (an app + a Postgres database)
backed up to a local restic repository.

> Running on **Docker Swarm**? See [`swarm/`](swarm/) for a swarm-mode stack
> (services + `deploy.labels`, node-pinned movers, manager placement).

sabon itself never runs restic. On a schedule it spawns ephemeral **mover**
containers (from its own image) that mount the sources read-only and run
restic. Movers run as **root** and sabon needs Docker API **write** access to
create them — hence the socket proxy has `POST=1`.

## Run

The onsite repo is a real host directory the movers bind-mount, so create it
first, then bring the stack up (set a repo password via the environment):

```sh
sudo mkdir -p /mnt/backup && sudo chown "$(id -u)" /mnt/backup

RESTIC_PASSWORD=change-me docker compose up -d --build
```

`SABON_RUN_ON_STARTUP=true` makes sabon back up once immediately, so there are
snapshots to look at without waiting for the 6-hourly schedule. Metrics and
health are on the host:

```sh
curl -fsS localhost:9333/healthz            # -> ok
curl -fsS localhost:9333/metrics | grep '^sabon_'
```

## What it demonstrates

- **One label, one logical app.** `app` carries `sabon.enable` and a
  `sabon.backup` document. Its own `appdata` volume is included automatically
  (`auto: true`); `/data/cache/**` is excluded.
- **A consistent database dump.** A `preHook` runs `pg_dump` **inside** the
  running `db` container (exec mode) into the shared `sabon-demo-dbdump`
  volume, which the label adds to the backup set via `extraVolumes:`. The live
  Postgres data dir (`pgdata`) is deliberately left out — the dump is the
  consistent source.
- **Per-app repository.** Everything lands in the `onsite` target at
  `/mnt/backup/demo`, tagged `demo` + `compose`.
- **Least-privilege API access.** sabon talks TCP to the socket proxy, never
  the raw socket. The proxy exposes exactly the endpoints sabon needs
  (`POST`, `CONTAINERS`, `IMAGES`, `VOLUMES`, `EXEC`, `EVENTS`).

Add the commented-out `offsite` target in [`targets.yaml`](targets.yaml) to
fan the same apps out to a remote R2/S3 repository.

## Verify

Show the discovered plan without backing anything up:

```sh
docker compose exec sabon sabon validate
```

List the snapshots written to the onsite repo:

```sh
docker compose exec sabon sabon snapshots --app demo --target onsite
```

Restore the latest snapshot into a staging directory (a safe, non-destructive
restore into a host dir — works even if the app is gone):

```sh
docker compose exec sabon sabon restore --app demo --target onsite --into /tmp/restore
```

You can also trigger a one-off backup instead of waiting for the schedule:

```sh
docker compose exec sabon sabon backup --app demo
```

## Tear down

```sh
docker compose down -v        # -v also drops the named volumes
sudo rm -rf /mnt/backup       # and the onsite repo, if you're done
```
