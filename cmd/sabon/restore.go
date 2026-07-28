package main

import (
	"fmt"
	"os"

	"github.com/davidborzek/sabon/api"
	"github.com/davidborzek/sabon/internal/backup"
	"github.com/davidborzek/sabon/internal/config"
	"github.com/davidborzek/sabon/internal/discovery"
	"github.com/urfave/cli/v2"
)

// runSnapshots lists snapshots for an app across one or all targets.
func runSnapshots(cCtx *cli.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := newLogger(cfg.LogLevel)
	cli, err := dockerClient()
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()

	ctx := cCtx.Context
	image, err := resolveImage(ctx, cli, cfg)
	if err != nil {
		return err
	}
	rt, err := newRuntime(ctx, cli, cfg, logger)
	if err != nil {
		return err
	}
	orch := newOrchestrator(cfg, image, rt, logger)
	app := cCtx.String("app")
	targets := cfg.Targets
	if name := cCtx.String("target"); name != "" {
		t, ok := cfg.Target(name)
		if !ok {
			return fmt.Errorf("unknown target %q", name)
		}
		targets = []api.Target{t}
	}
	var failed bool
	for _, t := range targets {
		fmt.Printf("== %s / %s ==\n", app, t.Name)
		if err := orch.RunSnapshots(ctx, app, t, os.Stdout); err != nil {
			logger.Error("snapshots failed", "app", app, "target", t.Name, "error", err)
			failed = true
		}
	}
	if failed {
		return fmt.Errorf("one or more snapshot listings failed")
	}
	return nil
}

// runCheckCmd runs `restic check` for an app across one or all targets.
func runCheckCmd(cCtx *cli.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := newLogger(cfg.LogLevel)
	cli, err := dockerClient()
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()

	ctx := cCtx.Context
	image, err := resolveImage(ctx, cli, cfg)
	if err != nil {
		return err
	}
	rt, err := newRuntime(ctx, cli, cfg, logger)
	if err != nil {
		return err
	}
	orch := newOrchestrator(cfg, image, rt, logger)
	app := cCtx.String("app")
	targets := cfg.Targets
	if name := cCtx.String("target"); name != "" {
		t, ok := cfg.Target(name)
		if !ok {
			return fmt.Errorf("unknown target %q", name)
		}
		targets = []api.Target{t}
	}
	var failed bool
	for _, t := range targets {
		fmt.Printf("== %s / %s ==\n", app, t.Name)
		if err := orch.RunCheck(ctx, app, t, os.Stdout); err != nil {
			logger.Error("check failed", "app", app, "target", t.Name, "error", err)
			failed = true
		}
	}
	if failed {
		return fmt.Errorf("one or more checks failed")
	}
	return nil
}

// runPruneCmd runs `restic prune` for an app across one or all targets.
func runPruneCmd(cCtx *cli.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := newLogger(cfg.LogLevel)
	cli, err := dockerClient()
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()

	ctx := cCtx.Context
	image, err := resolveImage(ctx, cli, cfg)
	if err != nil {
		return err
	}
	rt, err := newRuntime(ctx, cli, cfg, logger)
	if err != nil {
		return err
	}
	orch := newOrchestrator(cfg, image, rt, logger)
	app := cCtx.String("app")
	targets := cfg.Targets
	if name := cCtx.String("target"); name != "" {
		t, ok := cfg.Target(name)
		if !ok {
			return fmt.Errorf("unknown target %q", name)
		}
		targets = []api.Target{t}
	}
	var failed bool
	for _, t := range targets {
		fmt.Printf("== %s / %s ==\n", app, t.Name)
		if err := orch.RunPrune(ctx, app, t, os.Stdout); err != nil {
			logger.Error("prune failed", "app", app, "target", t.Name, "error", err)
			failed = true
		}
	}
	if failed {
		return fmt.Errorf("one or more prunes failed")
	}
	return nil
}

// runRestore restores a snapshot into a staging directory or, with --in-place,
// into the app's live volumes.
func runRestore(cCtx *cli.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := newLogger(cfg.LogLevel)
	cli, err := dockerClient()
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()

	app := cCtx.String("app")
	into := cCtx.String("into")
	inPlace := cCtx.Bool("in-place")
	if (into == "") == !inPlace {
		return fmt.Errorf("specify exactly one of --into <dir> or --in-place")
	}
	t, ok := cfg.Target(cCtx.String("target"))
	if !ok {
		return fmt.Errorf("unknown target %q", cCtx.String("target"))
	}

	ctx := cCtx.Context
	image, err := resolveImage(ctx, cli, cfg)
	if err != nil {
		return err
	}
	rt, err := newRuntime(ctx, cli, cfg, logger)
	if err != nil {
		return err
	}
	orch := newOrchestrator(cfg, image, rt, logger)
	opts := backup.RestoreOptions{
		Snapshot: cCtx.String("snapshot"),
		Into:     into,
		Stop:     cCtx.Bool("stop"),
		Include:  cCtx.StringSlice("include"),
	}

	var job *discovery.Job
	if inPlace {
		disc := rt.disc
		jobs, err := disc.List(ctx)
		if err != nil {
			return err
		}
		for i := range jobs {
			if jobs[i].App == app {
				job = &jobs[i]
				break
			}
		}
		if job == nil {
			return fmt.Errorf("no live container found for app %q (needed for --in-place; use --into <dir> instead)", app)
		}
	}
	return orch.RunRestore(ctx, app, job, t, opts, os.Stdout)
}
