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

	"github.com/davidborzek/sabon/internal/backup"
	"github.com/davidborzek/sabon/internal/config"
	"github.com/davidborzek/sabon/internal/mover"
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

// newOrchestrator builds the backup orchestrator with the zfs snapshotter image
// resolved once, so every entry point (daemon, backup, validate, snapshots,
// check, prune, restore) is consistent.
func newOrchestrator(cli client.APIClient, cfg *config.Config, image string, log *slog.Logger) *backup.Orchestrator {
	cfg.SnapshotZFSImage = zfsSnapshotterImage(cfg)
	return backup.New(cli, cfg, image, log)
}
