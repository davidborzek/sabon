# Deployment

sabon runs as a single long-running container (the orchestrator) that needs
**write** access to the Docker API — it creates, starts and removes mover
containers, mounts volumes into them, and execs hooks. This is a real privilege;
read [Privileges](#privileges) before exposing the socket.

!!! info "Scope"
    By default sabon targets **one Docker daemon** (standalone or Compose): it
    discovers containers and spawns movers on the daemon it connects to. On a
    Swarm **manager** it can instead drive the whole cluster via the service
    API — see [Docker Swarm](#swarm).

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

## Docker Swarm {#swarm}

!!! warning "Experimental"
    Swarm support is **early-stage**: it is exercised by a single-node
    `docker swarm init` end-to-end test in CI, but has **not been validated
    against a multi-node production cluster**. Treat it as beta, pin an image
    tag, and verify a restore before relying on it.

On a Swarm **manager**, sabon drives the cluster through the service API
(auto-detected, or forced with `SABON_RUNTIME=swarm`). It discovers **services**,
and for each backup spawns the mover as a **one-shot service pinned to the node
that holds the data** (a `node.hostname` constraint) — Swarm schedules it onto
the right node, so no per-node agent is needed. Cold backups scale the service
to zero and back; run history is the retained mover services. The behavioural
details and limits are in [Docker Swarm](configuration.md#docker-swarm).

Deploy sabon as a single `replicas: 1` service pinned to a manager node — it is
the one orchestrator for the whole cluster and is stateless (it re-discovers
each reconcile; history lives in the retained mover services):

```yaml
services:
  sabon:
    image: ghcr.io/davidborzek/sabon:latest
    deploy:
      replicas: 1
      placement:
        constraints: [node.role == manager]   # the service API is manager-only
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock   # a MANAGER node's socket
      - ./targets.yaml:/etc/sabon/targets.yaml:ro
    environment:
      SABON_RUNTIME: swarm          # optional: auto-detected on a manager
      RESTIC_PASSWORD: ${RESTIC_PASSWORD}
  # the apps you back up carry the sabon labels under their own deploy.labels:
  # myapp:
  #   deploy:
  #     labels:
  #       sabon.enable: "true"
  #       sabon.backup: |
  #         repo: myapp
```

Requirements and limits:

- **Manager placement** — pin sabon to a manager and give it that node's socket.
- **Remote repositories** — a local `path:` target is per-node and meaningless
  cluster-wide; use a remote backend (the cache volume stays per-node).
- **Labels go on `deploy.labels`** (the service), not container labels.
- **Replicated services only** for cold backups (`global`-mode scale-to-0 is a
  non-goal); **exec hooks** and **ZFS snapshots** are unsupported in swarm mode.

A single-node Swarm works the same way — one manager that is also the only node.

### Volumes & node placement {#swarm-volumes}

Swarm's default `local` volume driver is **per node** — not replicated. There are
two ways to run a stateful service, and sabon backs up both:

- **Pin to a node + local volume** — the simplest, and ideal for a single storage
  node. Give the service `replicas: 1` and a placement constraint
  (`node.hostname == …`, or a `node.labels.…` you set on the node), so its `local`
  volumes always live on that one node. Tradeoff: no failover — if that node is
  down, so is the service (its data lives only there).
- **Multi-node volume** — only needed when the service must reschedule across
  nodes: a CSI/cluster volume, an **NFS-backed `local` volume** (every node mounts
  the same export — the simplest homelab option), or a plugin driver
  (GlusterFS/Ceph/…). The data is then reachable wherever the task lands.

sabon pins each mover to where the data is: it reads the node of the service's
**running task** and constrains the mover there (`node.hostname==N`), so it mounts
the same node-local volume. For a **scaled-to-zero** service (no running task) it
falls back to the service's own `node.hostname`/`node.id`/`node.labels` placement
constraint, so a pinned service is still backed up on the right node while stopped.
A service with neither a running task nor a pinning constraint is left
unconstrained — the mover may land on a node without the volume, so pin such
services (or keep a replica running).

There is no reliable way to locate a bare `local` volume on another node from a
manager (the volume API is per-daemon), so pinning — via a running task or a
placement constraint — is how sabon finds the data. And since a local `path:`
repo is per-node, multi-node clusters must back up to a **remote** target.

### Replicated services {#swarm-replicas}

sabon backs up **one job per service**, not one per replica: it pins a single
mover to the node of one running task. This matches the per-app repository model
(one dataset per app) and avoids several movers writing the same repo at once.

- **`replicas: 1`** (pinned or not) — the instance is backed up. The normal case
  for stateful services.
- **`replicas > 1` on a shared volume** (CSI / NFS-backed / plugin) — every
  replica sees the same data, so the single backup covers them all.
- **`replicas > 1` with a per-node `local` volume each** — only **one** replica's
  volume is captured (which one is not deterministic); the others are silently
  skipped. Distinct per-replica local data is not a supported topology — use
  `replicas: 1` or a shared volume for anything stateful.

Note that a **cold** backup (`stop: true`) scales the *whole* service to zero, so
all replicas go down for the duration — another reason to keep stateful services
at `replicas: 1` or on a shared volume.

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

In **swarm mode** the object groups differ: sabon drives the service API, so it
needs `POST=1` with `SERVICES=1`, `TASKS=1` and `NODES=1` (plus `IMAGES=1`),
instead of `CONTAINERS`/`EXEC`. Point it at a **manager** node's socket.

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
