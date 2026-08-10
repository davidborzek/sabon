---
tags:
  - Labels
  - Configuration
---

# Labels

A container opts into backups with labels. The controlling label is
`sabon.backup`, a **document-form YAML** value that describes what to back up and
how. The label prefix is
configurable via `SABON_LABEL_PREFIX` (default `sabon`).

## Enabling a container

`sabon.enable`
:   `"true"` opts a container in. With `SABON_WATCH_BY_DEFAULT=true`, set `"false"` to opt a container **out** instead.

`sabon.instance`
:   Optional instance id. When you run multiple sabon orchestrators, each only manages containers whose `sabon.instance` matches its own — use it to partition ownership.

`sabon.backup`
:   The backup spec (document-form YAML). See below.

## The `sabon.backup` document

```yaml
labels:
  sabon.enable: "true"
  sabon.backup: |
    repo: paperless
    auto: true
    extraVolumes: [paperless-redis]
    excludeVolumes: [paperless-cache]
    extraPaths: [/srv/paperless/consume]
    exclude: ["*.tmp", "**/cache/**"]
    tags: [paperless, prod]
    targets:
      - onsite
      - name: offsite
        schedule: "0 0 4 * * *"
        retention: { daily: 14 }
        excludeVolumes: [paperless-redis]
    stop: false
    preHooks: []
    postHooks: []
```

### `repo`

*string · default: compose service, else container name* — The app / repo name. Determines the per-target repo (`<path>/<repo>` or `{app}` → `repo`); sibling containers of the same app should share one `repo`.

### `auto`

*bool · default `true`* — Include the labelled container's own bind mounts **and** named volumes automatically.

### `extraVolumes`

*list · optional* — Extra **named volumes** to include. May belong to sibling containers of the app (e.g. a database container's data volume).

### `excludeVolumes`

*list · optional* — **Named volumes to drop** from the auto-discovered set (`auto: true`) — e.g. a cache/scratch or live-DB volume you back up another way. Exact volume names; does not affect volumes you add via `extraVolumes`. Different from `exclude` (restic file patterns): this removes a whole **source**, not files within one.

### `extraPaths`

*list · optional* — Extra **host bind paths** to include.

### `exclude`

*list · optional* — restic `--exclude` patterns applied to the whole job (see [restic's excluding files](https://restic.readthedocs.io/en/stable/040_backup.html)).

### `tags`

*list · optional* — Extra restic tags added to snapshots (on top of sabon's own).

### `targets`

*list · default: all configured targets* — Which targets to back up to. Each entry is either a bare **target name** or an **object** with a `name` plus per-target overrides:

- `schedule` — cron override for this target (6-field, seconds first).
- `retention` — retention override for this target.
- `excludeVolumes` / `excludePaths` — named volumes / host bind paths dropped from *this* target's source set only (the source still goes to the other targets).
- `exclude` — extra restic `--exclude` patterns for this target.

Omit `targets` entirely to back up to every configured target with its defaults.

### `stop`

*bool · default `false`* — Stop the labelled container for the duration of the backup — a **cold backup**. Restarted afterwards.

### `snapshot`

*string · default `SABON_SNAPSHOT`* — Source snapshot strategy: `none` (mount live), `zfs` (atomic ZFS snapshot of each source's dataset; a non-ZFS source fails the backup), or `auto` (snapshot the ZFS sources, mount the rest live). See [Snapshots](backups.md#snapshots).

### `preHooks`

*list · optional* — Hooks run before the backup. See [Hooks](hooks.md).

### `postHooks`

*list · optional* — Hooks run after the backup. See [Hooks](hooks.md).

### `timeout`

*duration · default `SABON_BACKUP_TIMEOUT`* — Per-app backup deadline (e.g. `2h`); overrides the global default.

### `user`

*string · default `SABON_MOVER_USER`* — `uid[:gid]` the mover runs as, to match the app's volume ownership. Applies to backup and in-place restore.

### `groups`

*list · default `SABON_MOVER_GROUPS`* — Extra supplementary groups (GIDs) for the mover, to read group-owned source files. Applies to backup and in-place restore.


Each source ends up mounted read-only in the mover at `/data/<name>`, where
`<name>` is the volume name or a sanitized form of the bind path (this mapping
matters for restores — see [Restore](restore.md)).

## Worked examples

### A simple app (SQLite in a bind path)

Many self-hosted apps keep their entire state — including a SQLite database — in
one directory. `auto: true` captures the container's own mounts; there is nothing
else to do:

```yaml
services:
  linkding:
    image: ghcr.io/sissbruecker/linkding
    volumes:
      - /srv/linkding/data:/etc/linkding/data   # holds db.sqlite3
    labels:
      sabon.enable: "true"
      sabon.backup: |
        repo: linkding
        auto: true
        exclude:
          - "**/*.sqlite3-shm"               # skip SQLite WAL sidecars
          - "**/*.sqlite3-wal"
        tags: [sqlite]
```

SQLite is safe to back up live here as long as the WAL sidecars are excluded and
the DB is not mid-checkpoint; for a fully consistent copy use `stop: true` (cold
backup) or a pre-hook that runs `sqlite3 … ".backup"` into an included path.

### immich-style (media library volume + a DB dump volume)

A larger app spans several containers: the server (with the photo library
volume) and a Postgres container. Back up the **library** volume directly, but
never the live `pgdata` — instead dump the database into a small volume that is
*also* in the backup set, and exclude the live data dir. The dump is produced by
a pre-hook ([Hooks](hooks.md) shows both hook styles):

```yaml
services:
  immich-server:
    image: ghcr.io/immich-app/immich-server
    volumes:
      - immich-library:/usr/src/app/upload
    labels:
      sabon.enable: "true"
      sabon.backup: |
        repo: immich
        auto: true                       # picks up immich-library
        extraVolumes:
          - immich-dbdump                # the pg_dump target volume
        exclude:
          - "**/thumbs/**"               # regenerable
        tags: [immich, photos]
        preHooks:
          - image: postgres:17
            network: immich
            volumes: ["immich-dbdump:/dump"]
            env:
              PGPASSWORD: $${DB_PASSWORD}   # $$ so Compose passes it through; resolved from SABON_HOOK_ENV_DB_PASSWORD
            command:
              - sh
              - -c
              - "pg_dump -h immich-postgres -U immich immich > /dump/immich.sql"
            timeout: 10m

  immich-postgres:
    image: docker.io/tensorchord/pgvecto-rs:pg16-v0.2.0
    volumes:
      - immich-pgdata:/var/lib/postgresql/data   # NOT in the backup set

volumes:
  immich-library:
  immich-dbdump:
  immich-pgdata:
```

Here `immich-pgdata` is deliberately left out of the backup (the pre-hook writes
a consistent `.sql` into `immich-dbdump`, which *is* included). On restore you
recreate the database from the dump rather than from raw Postgres files. See
[Hooks](hooks.md) for the exec-mode variant of the same `pg_dump`.

## Validating and schema

These run inside the sabon container (e.g. `docker compose exec sabon /sabon validate`):

- `sabon validate` — discovers all jobs from the running containers and the
  targets file and prints the resulting plan **without backing up**. Use it to
  confirm a label change does what you expect.
- `sabon schema` — prints the JSON schema for the `sabon.backup` label ([`schemas/backup.json`](https://github.com/davidborzek/sabon/blob/main/schemas/backup.json)). The spec is embedded YAML inside a label, so it can't be wired to an editor live — see the [targets schema](configuration.md#schema) for a file you *can* validate.
- `sabon schema --targets` — prints the JSON schema for the targets file (see [Schema & editor validation](configuration.md#schema)).
