---
tags:
  - API
  - Automation
---

# HTTP API

An optional, token-authenticated HTTP control API exposes sabon's operations —
show config, list jobs and snapshots, trigger backup/check/prune/restore, and
browse run history.

It is **off by default**. Set [`SABON_API_ADDR`](configuration.md#sabon_api_addr)
to enable it and [`SABON_API_TOKEN`](configuration.md#sabon_api_token) to a
bearer token every request must carry. The token is optional but strongly
recommended — leaving it empty disables authentication (sabon warns at
startup). It is privileged — a `restore` overwrites live data — so bind it
to loopback and put TLS in front of it (a reverse proxy), the same way you would
expose anything else.

```yaml
services:
  sabon:
    environment:
      SABON_API_ADDR: ":8080"
      SABON_API_TOKEN: ${SABON_API_TOKEN}
    ports:
      - "127.0.0.1:8080:8080" # loopback; front with a TLS reverse proxy
```

## Authentication

Every request must send the token as a bearer:

```sh
curl -H "Authorization: Bearer $SABON_API_TOKEN" http://127.0.0.1:8080/api/v1/config
```

A missing or wrong token returns `401` (when a token is configured).

## Endpoints

All routes are under `/api/v1`. Responses are JSON unless noted.

### Reads

| Method | Path | Description |
| --- | --- | --- |
| `GET` | `/config` | Configured targets (secrets/credentials omitted). |
| `GET` | `/jobs` | Discovered backup jobs (app, sources, targets, snapshot mode). |
| `GET` | `/apps/{app}/snapshots?target=<name>` | restic snapshots for the app in a target (`target` required). |

### Triggers

Mutating operations are **fire-and-forget**: the `POST` returns `202 Accepted`
immediately and the work runs in the background. Watch it under
[run history](#run-history). `target` is optional for backup/check/prune (empty
= all of the app's targets).

| Method | Path | Description |
| --- | --- | --- |
| `POST` | `/apps/{app}/backup?target=<name>` | Back up the app. |
| `POST` | `/apps/{app}/check?target=<name>` | `restic check` (repo integrity). |
| `POST` | `/apps/{app}/prune?target=<name>` | `restic prune` (reclaim space). |
| `POST` | `/apps/{app}/restore` | Restore (JSON body below). |

`restore` body:

```jsonc
{
  "target": "onsite",       // required — which repo to restore from
  "snapshot": "latest",     // snapshot id; default "latest"
  "into": "/mnt/staging",   // host dir for a staging restore; omit for in-place
  "stop": true,             // stop the app during an in-place restore
  "include": ["/data/immich-dbdump"] // limit to matching paths (sources live under /data/<name>)
}
```

An API-triggered run is identical to a scheduled one — it takes the same locks
and records the same metrics and notifications.

## Run history

History is **durable**: sabon keeps the last
[`SABON_MOVER_HISTORY`](configuration.md#sabon_mover_history) (default `3`)
exited **mover containers** per app/target/action, so each run's status and full
restic **logs** live in Docker itself and survive a sabon restart. There is no
in-memory store — they are ordinary containers, so `docker ps -a` and
`docker logs <id>` see them too. In swarm mode the retained history is mover
*services* instead (`docker service ls` / `docker service logs`).

| Method | Path | Description |
| --- | --- | --- |
| `GET` | `/runs?app=<name>&target=<name>` | Recent runs, newest first (filters optional). |
| `GET` | `/runs/{id}` | One run (`id` is the mover container id). |
| `GET` | `/runs/{id}/logs` | The run's restic output (`text/plain`). |

A run:

```json
{
  "id": "9f3c…",
  "app": "immich",
  "target": "onsite",
  "action": "backup",
  "status": "succeeded",
  "exit_code": 0,
  "started_at": "2026-07-27T21:00:00Z",
  "finished_at": "2026-07-27T21:00:08Z"
}
```

`status` is `running`, `succeeded` or `failed`. A failure that happens **before**
a mover starts (e.g. a snapshot or pre-hook error) produces no run record — it is
reported via logs, [notifications](metrics.md#notifications) and metrics instead.

## Example

```sh
export T="Authorization: Bearer $SABON_API_TOKEN"
base=http://127.0.0.1:8080/api/v1

curl -H "$T" "$base/jobs"
curl -H "$T" -X POST "$base/apps/immich/backup"     # -> 202
curl -H "$T" "$base/runs?app=immich"                # find the run + its id
curl -H "$T" "$base/runs/<id>/logs"                 # restic output
```
