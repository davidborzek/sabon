# Hooks

Hooks let you run a command around a backup — most often to produce a consistent
database dump *before* restic reads the files. They are declared in the
`sabon.backup` label as `preHooks` (run before the backup) and `postHooks` (run
after it). See [Labels](labels.md) for where they sit.

A hook always has a `command` (argv, **required**) plus optional fields
depending on its mode.

## Two modes

### exec mode (default)

Runs the command **inside an existing container** with `docker exec` — by default
the labeled container. Use it to reach a tool that already lives in the app's
container (its database client, `sqlite3`, etc.).

`command`
:   argv to execute (required).

`container`
:   Container to exec in. Default: the labeled container.

`user`
:   User to exec as (e.g. `postgres`).

`env`
:   Environment map. A value `${NAME}` resolves from the labelled container's own environment, then the `SABON_HOOK_ENV_<NAME>` allowlist (never sabon's full environment); in a Compose file escape it as `$${NAME}`. See [Credentials in hooks](#credentials-in-hooks).

`timeout`
:   Go duration (e.g. `5m`). Empty = no timeout.

### run mode

Spawns a **fresh one-shot container** from `image` instead of exec-ing into an
existing one. Set `image` to switch a hook into run mode. Use it when the tool
you need isn't in the app's container, or when you want a clean, pinned tool
image.

`command`
:   argv to execute (required).

`image`
:   Image to spawn (presence selects run mode).

`network`
:   Docker network to attach to, so the container can reach services by name.

`volumes`
:   Mounts, `["source:/target[:ro]"]`; `source` is a volume name or a host path.

`env`
:   Environment map. A value `${NAME}` resolves from the labelled container's own environment, then the `SABON_HOOK_ENV_<NAME>` allowlist (never sabon's full environment); in a Compose file escape it as `$${NAME}`. See [Credentials in hooks](#credentials-in-hooks).

`timeout`
:   Go duration (e.g. `5m`). Empty = no timeout.

## The database-dump pattern

The canonical use is a **consistent database backup**: a pre-hook dumps the
database into a volume that is *also* in the backup set, so the mover captures the
dump as an ordinary file. The live database data directory is **excluded** from
the backup (backing up raw, changing DB files is not crash-consistent).

The moving parts:

1. A small named volume (e.g. `app-dbdump`) listed in the label's `extraVolumes`.
2. A pre-hook that writes `dump.sql` into that volume.
3. The live `pgdata` volume left **out** of the backup set (or excluded).

Below are both hook modes doing the same `pg_dump`.

### exec mode — dump inside the app's Postgres container

`docker exec`s into the existing `myapp-postgres` container and dumps into a path
that is a mounted dump volume shared with the backup set:

```yaml
labels:
  sabon.enable: "true"
  sabon.backup: |
    repo: myapp
    auto: true
    extraVolumes:
      - myapp-dbdump          # mounted at /dump inside myapp-postgres too
    exclude:
      - "**/pgdata/**"        # never back up the live data dir
    preHooks:
      - container: myapp-postgres
        user: postgres
        command:
          - sh
          - -c
          - "pg_dump -U myapp myapp > /dump/myapp.sql"
        timeout: 10m
```

This assumes `myapp-postgres` has the `myapp-dbdump` volume mounted (e.g. at
`/dump`) so the dump lands somewhere the backup includes.

### run mode — throwaway `postgres:17` over the network

No dump path inside the app container? Spawn a pinned `postgres:17` container,
attach it to the app's network so it can reach the DB by service name, and mount
the shared dump volume into it:

```yaml
labels:
  sabon.enable: "true"
  sabon.backup: |
    repo: myapp
    auto: true
    extraVolumes:
      - myapp-dbdump          # the shared dump volume, in the backup set
    exclude:
      - "**/pgdata/**"
    preHooks:
      - image: postgres:17
        network: myapp        # reach "myapp-postgres" by name
        volumes:
          - "myapp-dbdump:/dump"
        env:
          PGPASSWORD: $${DB_PASSWORD}   # $$ escapes Compose; sabon resolves it from SABON_HOOK_ENV_DB_PASSWORD (see below)
        command:
          - sh
          - -c
          - "pg_dump -h myapp-postgres -U myapp myapp > /dump/myapp.sql"
        timeout: 10m
```

Both variants leave `myapp-dbdump` holding a fresh, consistent `myapp.sql` at the
moment the mover runs `restic backup`, and both keep the volatile `pgdata` out of
the repository. On restore you recreate the database by loading `myapp.sql` (see
[Restore](restore.md)).

### Credentials in hooks

**Prefer exec mode.** An exec hook runs *inside* an existing container, which
already has its own credentials in its environment, so no secret has to be
injected by sabon at all (the exec example above uses the DB container's own
`$$POSTGRES_USER`). Reach for run-mode env injection only when the dump runs in
a throwaway container that lacks those credentials.

When a hook `env:` value does interpolate `${NAME}`, sabon resolves it in this
order — never from sabon's full environment:

1. the **labelled container's own environment** (the label author owns it), then
2. the **`SABON_HOOK_ENV_<NAME>`** allowlist in sabon's environment, for a secret
   that lives nowhere on the app's container (e.g. only in a sibling database).

So a label can read the app's own env and the values you deliberately expose,
but never sabon's `RESTIC_PASSWORD` or another target's credentials.

In a Compose file, escape the reference as `$${NAME}` so Compose passes the
literal `${NAME}` through to sabon instead of interpolating it itself (the same
reason the exec `command` above uses `$$POSTGRES_USER`):

```yaml
    # in the sabon.backup label
    env: { PGPASSWORD: "$${DB_PASSWORD}" }
```

```yaml
# only if the value is not already on the app's container: expose it on sabon
services:
  sabon:
    environment:
      SABON_HOOK_ENV_DB_PASSWORD: ${DB_PASSWORD}   # ordinary Compose interpolation
```

Either way the secret stays in an environment, never in the label. Outside
Compose (no interpolation) use a single `$`.

## Tips

- Keep dumps small and fast; a hook `timeout` guards against a wedged dump
  hanging the whole backup.
- Prefer exec mode for dumps — it uses the app container's own credentials, so
  no secret is injected. For run mode, hook `${NAME}` resolves from the app
  container's env then `SABON_HOOK_ENV_<NAME>`; see [Credentials in hooks](#credentials-in-hooks).
- A failing pre-hook fails the backup for that job (restic never runs against an
  incomplete dump); check the notification and `sabon_backups_total{result="error"}`.
- `postHooks` run after the backup completes — useful for cleanup or a
  success ping, but the database-consistency work belongs in `preHooks`.
