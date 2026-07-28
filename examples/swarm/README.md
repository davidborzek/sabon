# Example — Docker Swarm

sabon running in **swarm mode** on a single-node cluster: it drives the Docker
service API from a manager, discovers a labelled **service**, and backs it up by
spawning a one-shot **mover service** pinned to the node that holds the data.

For the standalone (single Docker host) demo, see [`../`](../).

## Run

```sh
docker swarm init                                  # if this host isn't a swarm yet
sudo mkdir -p /mnt/backup && sudo chown "$(id -u)" /mnt/backup

RESTIC_PASSWORD=change-me docker stack deploy -c docker-stack.yaml sabon-demo
```

`SABON_RUN_ON_STARTUP=true` backs up once on deploy, so there are snapshots to
look at without waiting for the schedule. Metrics/health are published on the
host via the routing mesh:

```sh
curl -fsS localhost:9333/healthz            # -> ok
```

## What it demonstrates

- **Swarm auto-detection.** `SABON_RUNTIME=swarm` (or omit it — a manager is
  auto-detected via `docker info`).
- **Labels on the service.** The `sabon.enable` / `sabon.backup` labels sit under
  the app service's `deploy.labels`, not on the container.
- **Node-pinned movers.** sabon resolves the node the app's task runs on and
  creates the mover as a `replicas: 1`, restart-none service constrained to that
  node (`node.hostname==…`), so it reaches the node-local `appdata` volume. Watch
  them appear and vanish: `docker service ls`.
- **Manager placement.** sabon itself is a `replicas: 1` service pinned to a
  manager (the service API is manager-only) with that node's Docker socket.

Not shown (unsupported in swarm): **exec-mode hooks** (a manager can't exec into
a task on another node — use a run-mode hook) and **ZFS snapshots** (the
snapshotter is node-local). For a real multi-node cluster, back up to a
**remote** target — a local `path:` repo is per-node; see
[`targets.yaml`](targets.yaml).

## Verify

sabon's CLI runs inside its task container. Grab it, then invoke `sabon`:

```sh
sid=$(docker ps -q -f name=sabon-demo_sabon | head -1)

docker exec "$sid" sabon validate                                   # the discovered plan
docker exec "$sid" sabon snapshots --app demo --target onsite       # snapshots written
docker exec "$sid" sabon restore  --app demo --target onsite --into /tmp/restore
docker exec "$sid" sabon backup   --app demo                        # one-off, off-schedule
```

## Tear down

```sh
docker stack rm sabon-demo
docker swarm leave --force     # if you initialised the swarm just for this demo
sudo rm -rf /mnt/backup        # and the onsite repo, if you're done
```
