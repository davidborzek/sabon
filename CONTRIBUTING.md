# Contributing

Thanks for your interest in sabon! Contributions are welcome.

## Development

sabon is a standard Go module (Go 1.26+). No code generation or extra
tooling is required.

```sh
go build ./...      # build
go vet ./...        # vet
go test ./...       # unit tests
```

Formatting is enforced in CI — run `gofmt -w .` before committing.

### End-to-end tests

Unit tests run with a plain `go test ./...`. A full end-to-end tier drives real
infrastructure via Docker (behind a build tag, skipped automatically when Docker
is unavailable): it builds the sabon binary and image and backs a labelled demo
container up to a real restic repository, then lists and restores it.

```sh
go test -tags e2e ./test/e2e/
```

### Extending sabon

sabon discovers containers carrying backup labels, then for each one spawns an
ephemeral mover container that runs restic to back up the container's volumes to
a configured repository, on a schedule. The pieces are deliberately small and
composable:

- `internal/label` — the container label schema (`sabon.backup`) and parsing.
- `internal/config` — process (`SABON_*`) and targets-file configuration.
- `internal/discovery` — the shared backup-job domain types (`Job`/`Source`) and the source-resolution helpers.
- `internal/mover` — the mover contract and the restic-running mover side (spawning lives in `internal/engine`).
- `internal/engine` — the runtime seam: `Engine`/`Discoverer`/`Quiescer`/`Hooks`, implemented per runtime in `internal/engine/docker` (containers) and `internal/engine/swarm` (services); the composition root in `cmd/sabon` selects the set via `docker info` / `SABON_RUNTIME`.
- `internal/backup` — orchestrates a run (hooks + mover + per-repo lock) and restore/snapshots.
- `internal/scheduler` — drives backups on their configured cron schedules.
- `internal/reconcile` — turns discovered containers into scheduled jobs and records metrics/notifications.
- `internal/notify` — optional shoutrrr notifications.
- `internal/metrics` — Prometheus metrics and the health endpoints.

See the [README](README.md) and the existing implementations under these
packages.

## Pull requests

- Keep changes focused; one logical change per PR.
- Use [Conventional Commits](https://www.conventionalcommits.org/) for commit
  messages (`feat:`, `fix:`, `docs:`, `refactor:`, `ci:` …).
- Add or update tests for behavioural changes.
- Make sure `gofmt`, `go vet`, and `go test ./...` pass.

## Reporting issues

Use the issue templates. For security-sensitive reports, see
[SECURITY.md](SECURITY.md).

## Releases

Releases are automated — no manual tagging:

- **[release-please](https://github.com/googleapis/release-please)** watches
  `main` and, from the Conventional Commit history, maintains a "release PR"
  that bumps the version and updates `CHANGELOG.md`. Merging it creates the tag
  and the GitHub release.
- **[goreleaser](https://goreleaser.com/)** then builds the binaries and the
  multi-arch (`amd64`/`arm64`) image, pushes it to
  `ghcr.io/davidborzek/sabon`, and attaches archives + checksums to the
  release — in the same workflow run (hanging off release-please's output, so no
  PAT is needed).
