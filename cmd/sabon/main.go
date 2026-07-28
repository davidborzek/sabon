// Command sabon is a label-driven restic backup orchestrator for Docker. It
// discovers containers labelled with a backup spec, and on a schedule spawns
// ephemeral "mover" containers that back their volumes up to per-app restic
// repositories (onsite and/or offsite).
//
// Subcommands live in sibling files: daemon.go (default), backup.go,
// restore.go (snapshots + restore), inspect.go (mover/schema/validate).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	_ "time/tzdata" // embed the IANA tz database so TZ works on the distroless image (cron schedules + log timestamps honour TZ; default UTC)

	"github.com/davidborzek/sabon/internal/backup"
	"github.com/davidborzek/sabon/internal/config"
	"github.com/davidborzek/sabon/internal/engine"
	"github.com/davidborzek/sabon/internal/engine/docker"
	"github.com/davidborzek/sabon/internal/engine/swarm"
	"github.com/davidborzek/sabon/internal/mover"
	"github.com/davidborzek/sabon/internal/snapshot"
	"github.com/davidborzek/sabon/internal/snapshot/providers/zfs"
	"github.com/docker/docker/client"
	"github.com/urfave/cli/v2"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	app := &cli.App{
		Name:    "sabon",
		Usage:   "Label-driven restic backup orchestrator for Docker",
		Version: version,
		Action:  runDaemon,
		Commands: []*cli.Command{
			{Name: "mover", Hidden: true, Usage: "internal: run a single restic operation (executed inside a mover container)", Action: runMover},
			{Name: "backup", Usage: "Run all backups once and exit", Action: runBackupOnce, Flags: []cli.Flag{
				&cli.StringFlag{Name: "app", Usage: "only back up this app"},
			}},
			{Name: "validate", Usage: "Discover jobs and print the plan without backing up", Action: runValidate},
			{Name: "snapshots", Usage: "List snapshots for an app", Action: runSnapshots, Flags: []cli.Flag{
				&cli.StringFlag{Name: "app", Usage: "app (repo) name", Required: true},
				&cli.StringFlag{Name: "target", Usage: "target name (default: all targets)"},
			}},
			{Name: "restore", Usage: "Restore a snapshot (staging by default; --in-place writes into live volumes)", Action: runRestore, Flags: []cli.Flag{
				&cli.StringFlag{Name: "app", Usage: "app (repo) name", Required: true},
				&cli.StringFlag{Name: "target", Usage: "target name", Required: true},
				&cli.StringFlag{Name: "snapshot", Usage: "snapshot ID", Value: "latest"},
				&cli.StringFlag{Name: "into", Usage: "restore into this host directory (staging; safe)"},
				&cli.BoolFlag{Name: "in-place", Usage: "restore into the app's live volumes (destructive)"},
				&cli.BoolFlag{Name: "stop", Usage: "stop the app during an in-place restore"},
				&cli.StringSliceFlag{Name: "include", Usage: "limit the restore to matching paths"},
			}},
			{Name: "check", Usage: "Run restic repository check for an app", Action: runCheckCmd, Flags: []cli.Flag{
				&cli.StringFlag{Name: "app", Usage: "app (repo) name", Required: true},
				&cli.StringFlag{Name: "target", Usage: "target name (default: all targets)"},
			}},
			{Name: "prune", Usage: "Run restic prune for an app (reclaim space)", Action: runPruneCmd, Flags: []cli.Flag{
				&cli.StringFlag{Name: "app", Usage: "app (repo) name", Required: true},
				&cli.StringFlag{Name: "target", Usage: "target name (default: all targets)"},
			}},
			{Name: "schema", Usage: "Print the JSON schema for the backup label", Action: runSchema, Flags: []cli.Flag{
				&cli.BoolFlag{Name: "targets", Usage: "print the targets-file schema instead of the label schema"},
			}},
		},
	}
	if err := app.Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "sabon:", err)
		os.Exit(1)
	}
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}

func dockerClient() (*client.Client, error) {
	return client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
}

// resolveImage determines the mover image: the configured value, else the exact
// image sabon was deployed as (auto-detected inside Docker — registry, digest,
// fork and all), else the official image matching this binary's version.
func resolveImage(ctx context.Context, cli client.APIClient, cfg *config.Config) (string, error) {
	if cfg.MoverImage != "" {
		return cfg.MoverImage, nil
	}
	// In Docker: reuse exactly what sabon was deployed as (most accurate).
	if img, err := mover.DetectSelfImage(ctx, cli); err == nil {
		return img, nil
	}
	// Not in Docker (or inspect failed): fall back to the version-matched image.
	if version != "dev" {
		return "ghcr.io/davidborzek/sabon:" + version, nil
	}
	return "", fmt.Errorf("could not determine mover image; set SABON_MOVER_IMAGE")
}

// zfsSnapshotterImage returns the configured privileged snapshotter image, else
// the build-matched default so the snapshotter tracks sabon's own version. A
// "dev" build (no version stamped in) has no default and returns "".
func zfsSnapshotterImage(cfg *config.Config) string {
	if cfg.SnapshotZFSImage != "" {
		return cfg.SnapshotZFSImage
	}
	if version == "dev" {
		// Unstamped/dev build: do not silently pull a floating :latest privileged
		// image. Require an explicit SABON_SNAPSHOT_ZFS_IMAGE to use ZFS snapshots.
		return ""
	}
	return "ghcr.io/davidborzek/sabon/zfs-snapshotter:" + version
}

// newOrchestrator builds the backup orchestrator from the resolved runtime
// (engine, hooks, quiescer, host and snapshot providers).
func newOrchestrator(cfg *config.Config, image string, rt *runtime, log *slog.Logger) *backup.Orchestrator {
	return backup.New(cfg, image, rt.eng, rt.hooks, rt.quiesce, rt.host, rt.snaps, log)
}

// runtime bundles the runtime-specific implementations the composition root
// wires in: standalone Docker or Docker Swarm, chosen by detectRuntime.
type runtime struct {
	mode    string
	eng     engine.Engine
	disc    engine.Discoverer
	quiesce engine.Quiescer
	hooks   engine.Hooks
	host    engine.Host
	snaps   []snapshot.Snapshotter
}

// newRuntime detects whether sabon drives a standalone Docker host or a Swarm
// manager and builds the matching engine, discoverer, quiescer, hooks and
// snapshot providers (snapshots are a Docker-only capability).
func newRuntime(ctx context.Context, cli client.APIClient, cfg *config.Config, log *slog.Logger) (*runtime, error) {
	mode, err := detectRuntime(ctx, cli, cfg.Runtime)
	if err != nil {
		return nil, err
	}
	log.Info("runtime detected", "mode", mode)
	rt := &runtime{mode: mode}
	switch mode {
	case "swarm":
		rt.eng = swarm.New(cli)
		rt.disc = swarm.NewDiscoverer(cli, cfg.LabelPrefix, cfg.WatchByDefault, cfg.CacheVolume, cfg.Instance, log)
		rt.quiesce = swarm.NewQuiescer(cli, cfg.Instance)
		rt.hooks = swarm.NewHooks(cli)
		rt.host = swarm.NewHost()
		// Snapshots are unsupported in swarm: the ZFS snapshotter is a
		// privileged, node-local container that cannot run as a node-pinned
		// service. rt.snaps stays nil, so "auto" mounts sources live while a
		// strict "zfs" default fails every backup — warn accordingly.
		switch cfg.Snapshot {
		case "auto":
			log.Warn("SABON_SNAPSHOT=auto has no snapshot effect in swarm mode; sources are backed up live (snapshots are unsupported)", "snapshot", cfg.Snapshot)
		case "none":
		default:
			log.Warn("SABON_SNAPSHOT is a strict snapshot mode unsupported in swarm mode; backups defaulting to it will fail — set 'auto' to back up live, or 'none'", "snapshot", cfg.Snapshot)
		}
	default:
		rt.eng = docker.New(cli)
		rt.disc = docker.NewDiscoverer(cli, cfg.LabelPrefix, cfg.WatchByDefault, cfg.CacheVolume, cfg.Instance, log)
		rt.quiesce = docker.NewQuiescer(cli)
		rt.hooks = docker.NewHooks(cli)
		rt.host = docker.NewHost(cli)
		// The ZFS snapshotter is a standalone-Docker capability (a privileged
		// local container); only the docker runtime provides it.
		cfg.SnapshotZFSImage = zfsSnapshotterImage(cfg)
		rt.snaps = []snapshot.Snapshotter{zfs.New(cli, cfg.SnapshotZFSImage, cfg.Instance, log)}
	}
	return rt, nil
}

// detectRuntime honours the SABON_RUNTIME override, else auto-detects: a Swarm
// manager (control available) uses the swarm runtime, anything else standalone.
func detectRuntime(ctx context.Context, cli client.APIClient, override string) (string, error) {
	switch override {
	case "standalone", "swarm":
		return override, nil
	case "", "auto":
		info, err := cli.Info(ctx)
		if err != nil {
			return "", fmt.Errorf("detect runtime (docker info): %w", err)
		}
		if info.Swarm.ControlAvailable {
			return "swarm", nil
		}
		return "standalone", nil
	default:
		return "", fmt.Errorf("SABON_RUNTIME must be \"auto\", \"standalone\" or \"swarm\", got %q", override)
	}
}

// recoverQuiesced brings back any app a previous run stranded at zero replicas
// by crashing between a cold backup's Stop and Start. No-op on standalone.
func (rt *runtime) recoverQuiesced(ctx context.Context, log *slog.Logger) {
	q, ok := rt.quiesce.(engine.Recoverer)
	if !ok {
		return
	}
	if n, err := q.RecoverQuiesced(ctx); err != nil {
		log.Warn("recover quiesced services failed", "error", err)
	} else if n > 0 {
		log.Info("recovered quiesced services", "count", n)
	}
}
