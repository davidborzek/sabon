# Security policy

## Reporting a vulnerability

Please **do not** open a public issue for security vulnerabilities.

Instead, report them privately via GitHub's
[security advisories](https://github.com/davidborzek/sabon/security/advisories/new)
("Report a vulnerability"). You will receive a response as soon as possible, and
disclosure will be coordinated with you.

## Threat model

sabon is a **privileged component**; read this before exposing it.

### Trust boundary

sabon needs **write** access to the Docker API — it creates, starts and removes
mover containers and mounts arbitrary host volumes into them. A client that can
do this can start a container with any bind mount and thereby read or write **any
file on the Docker host, as root**. Treat the Docker host sabon runs on as the
trust boundary: anyone who can reach sabon's control surface *or* its Docker
socket has, in effect, root on that host. In swarm mode this is a **manager**.

### Assets

- **restic repository passwords** (`RESTIC_PASSWORD`) — decrypt every backup.
- **Backend credentials** (S3/R2/WebDAV/… keys) — read and write the offsite copy.
- **Application data** — the movers mount every source volume and can read it.
- **The repositories** — encrypted at rest, but availability- and
  integrity-sensitive (a backup you cannot restore is worthless).

### What an attacker who reaches sabon can do

- Spawn a mover with an arbitrary bind mount → read or modify host files as root.
- Read the repository passwords and backend credentials from sabon's environment.
- Delete or corrupt backups via the backend credentials or `forget`/`prune`.

### Mitigations

- **Run on a trusted host only**, and do not expose the Docker socket or sabon's
  HTTP API to untrusted networks. Bind the metrics/API endpoint to loopback or a
  private network.
- **Narrow the socket.** Prefer a [socket proxy](docs/deployment.md#socket) that
  allows only `POST` plus the object groups sabon drives, instead of the raw
  socket. The proxy limits *which* endpoints are reachable, not what those
  endpoints can do — it narrows the privilege but does not remove it.
- **Keep the orchestrator nonroot**; only the short-lived movers run as root, and
  only for the duration of a backup — see [Privileges](docs/deployment.md#privileges).
- **Inject secrets at runtime** from a secret store or the environment; keep
  `RESTIC_PASSWORD` and backend keys out of version control and image layers.
- **Protect the backend independently.** Where the backend supports it, use
  append-only / immutable storage or a separate prune credential, so a
  compromised sabon cannot erase every copy.
