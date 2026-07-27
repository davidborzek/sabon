# Deployment

sabon runs as a single long-running container (the orchestrator) that needs
**write** access to the Docker API — it creates, starts and removes mover
containers, mounts volumes into them, and execs hooks. This is a real privilege;
read [Privileges](#privileges) before exposing the socket.

!!! info "Scope: a single Docker host"
    sabon targets **one Docker daemon** (standalone or Compose): it discovers
    containers and spawns movers on the daemon it connects to. It is not a
    cluster orchestrator.

## Docker Swarm {#swarm}

sabon has no cluster-wide view, but it composes with Swarm in a **node-local**
way: deploy it as a `global` service (one task per node). Each instance then
talks to its own node's daemon, discovers the Swarm **task containers scheduled
on that node** (they carry the service's container labels), and spawns movers
locally against their node-local volumes — no code changes needed. A single-node
Swarm behaves just like standalone.

Caveats of the node-local model:

- A mover is a plain node-local container, so it only reaches volumes on **its
  own node**. A service's data must live where its task runs (node-local volumes
  or a pinned placement) — the normal case for stateful services.
- The snapshot host is currently the app name, and the per-repo lock is
  per-process (not cluster-wide). So if the *same* app is **replicated across
  nodes** and backs up to a *shared* repository, snapshots and restic locks from
  different nodes will collide. Prefer node-local repositories, single-replica
  stateful services, or per-node target paths in that case.

What sabon deliberately does **not** do: read the Swarm services/tasks API from a
manager, schedule movers onto other nodes, or coordinate a cluster-wide
repository. Volume locality makes backups inherently node-bound, so that
cross-node orchestration is a non-goal.

## Docker Compose

Images live at `ghcr.io/davidborzek/sabon`. `latest` and `X.Y.Z`/`X.Y` track
stable releases; `main` (and `sha-<commit>`) are rolling **edge** builds of the
latest `main` for trying unreleased changes.

```yaml
name: sabon

services:
  sabon:
    image: ghcr.io/davidborzek/sabon:latest
    user: "1000:1000"                 # the orchestrator itself can run nonroot
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock   # Docker API (read + WRITE)
      - ./targets.yaml:/etc/sabon/targets.yaml:ro
      - /mnt/backup:/mnt/backup       # local target backend(s)
    environment:
      SABON_CONFIG: /etc/sabon/targets.yaml
      SABON_METRICS_ADDR: ":9333"
      SABON_LOG_LEVEL: info
      SABON_MOVER_USER: "0:0"         # movers run as root (default)
      RESTIC_PASSWORD: ${RESTIC_PASSWORD}
      # remote backend creds referenced by targets.yaml env:
      R2_ENDPOINT: ${R2_ENDPOINT}
      R2_BUCKET: ${R2_BUCKET}
      R2_ACCESS_KEY_ID: ${R2_ACCESS_KEY_ID}
      R2_SECRET_ACCESS_KEY: ${R2_SECRET_ACCESS_KEY}
    ports:
      - "127.0.0.1:9333:9333"         # metrics/health, bind to loopback
    restart: unless-stopped

volumes:
  sabon-cache:                        # shared restic cache (SABON_CACHE_VOLUME)
```

A few things worth calling out:

- **Local target backends must be visible to *movers*, not just the
  orchestrator.** sabon bind-mounts the same host path into each mover, so a local
  target `path: /mnt/backup` must be a real host path (mount it into the
  orchestrator too if you want `sabon validate`/logs to see it).
- **`SABON_MOVER_IMAGE`** auto-detects sabon's own image when running in Docker.
  If you run the orchestrator outside Docker, set it explicitly.
- **The cache volume** (`SABON_CACHE_VOLUME`, default `sabon-cache`) is created on
  demand and shared across movers; declaring it keeps it under compose's
  lifecycle.

## Plain Docker

The same setup without Compose — a single `docker run` for the orchestrator (the
cache volume and the movers are created on demand):

```sh
docker run -d --name sabon \
  --user 1000:1000 \
  --restart unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$PWD/targets.yaml:/etc/sabon/targets.yaml:ro" \
  -v /mnt/backup:/mnt/backup \
  -e SABON_CONFIG=/etc/sabon/targets.yaml \
  -e SABON_METRICS_ADDR=":9333" \
  -e SABON_MOVER_USER="0:0" \
  -e RESTIC_PASSWORD="$RESTIC_PASSWORD" \
  -p 127.0.0.1:9333:9333 \
  ghcr.io/davidborzek/sabon:latest
```

Remote-backend credentials referenced by `targets.yaml` (e.g. `R2_*`, `AWS_*`)
go in as further `-e` flags. The [Compose caveats](#docker-compose) apply
identically: a local target `path:` must be a real host path (movers bind-mount
the same path), and mounting the raw socket is a real privilege — see
[socket access](#socket) and [Privileges](#privileges).

## Docker socket access {#socket}

sabon needs the Docker API with **write** enabled. It must be able to:

- **create / start / remove** containers (the movers),
- **mount volumes** into them,
- **exec** into containers (hooks in exec mode),
- read **containers/images/volumes** and stream **events**.

There are two supported ways to grant this.

### Option A — raw socket on a trusted node

Simplest, and appropriate on a host you already trust:

```yaml
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
```

Mounting the raw socket grants full control of the host. Only do this where that
is acceptable.

### Option B — socket proxy with POST enabled

To narrow the surface, front the socket with
[`tecnativa/docker-socket-proxy`](https://github.com/Tecnativa/docker-socket-proxy)
and point sabon at it via `DOCKER_HOST`. sabon needs **`POST=1`** plus the
object groups it drives:

```yaml
services:
  docker-socket-proxy:
    image: ghcr.io/tecnativa/docker-socket-proxy:latest
    environment:
      POST: 1          # sabon must create/start/remove containers
      CONTAINERS: 1    # list/inspect/create/start/remove
      IMAGES: 1        # pull/inspect the mover image
      VOLUMES: 1       # mount volumes into movers
      EXEC: 1          # exec-mode hooks
      EVENTS: 1        # discovery event stream
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    # expose only to sabon, e.g. on a private network

  sabon:
    image: ghcr.io/davidborzek/sabon:latest
    environment:
      DOCKER_HOST: tcp://docker-socket-proxy:2375
      # …rest as above; drop the docker.sock mount
    depends_on: [docker-socket-proxy]
```

Even with the proxy, `POST=1` is a meaningful grant — a client that can create
containers and mount arbitrary volumes can effectively reach the host. The proxy
limits *which* endpoints are reachable, not what those endpoints can do.

## Privileges {#privileges}

Two distinct privileges are in play; keep them separate in your head:

- **The orchestrator** can run **nonroot** (`user: 1000:1000`). It only talks to
  the Docker API — it never touches app data or repos directly.
- **The movers** run **as root** by default (`SABON_MOVER_USER=0:0`) because they
  must read arbitrary application data (owned by many uids) and write
  repositories. This is inherent to a backup tool that reads everything; narrow
  it with `SABON_MOVER_USER` only if every source and repo is readable/writable
  by a specific uid — or per app with the `user` label, to match that app's
  volume ownership.

So the daemon you leave running can be unprivileged; the short-lived movers it
spawns are the privileged part, and they exist only for the duration of a backup.

## Observability

With `SABON_METRICS_ADDR` set (default `:9333`), sabon serves `/metrics`
(Prometheus), `/healthz` (liveness) and `/readyz` (readiness). Bind it to
loopback or a private interface and scrape it; wire the health endpoints into
your orchestrator's probes. Full details, the metric reference and PromQL are in
[Metrics](metrics.md); ready-made Grafana dashboards and Prometheus alerts
live in [`dashboards/`](https://github.com/davidborzek/sabon/tree/main/dashboards).

For failure notifications beyond metrics, set `SABON_NOTIFY_URLS` (shoutrrr) — see
[Notifications](metrics.md#notifications).
