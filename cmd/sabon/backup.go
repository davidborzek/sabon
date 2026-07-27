package main

import (
	"github.com/davidborzek/sabon/internal/config"
	"github.com/davidborzek/sabon/internal/discovery"
	"github.com/davidborzek/sabon/internal/metrics"
	"github.com/davidborzek/sabon/internal/notify"
	"github.com/davidborzek/sabon/internal/reconcile"
	"github.com/davidborzek/sabon/internal/scheduler"
	"github.com/urfave/cli/v2"
)

// runBackupOnce runs every discovered job against its targets once.
func runBackupOnce(cCtx *cli.Context) error {
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

	m := metrics.New(version)
	orch := newOrchestrator(cli, cfg, image, logger)
	if _, err := orch.Reap(ctx); err != nil {
		logger.Warn("reap orphan movers failed", "error", err)
	}
	if _, err := orch.ReapSnapshotter(ctx); err != nil {
		logger.Warn("reap orphan snapshots failed", "error", err)
	}
	disc := discovery.New(cli, cfg.LabelPrefix, cfg.WatchByDefault, cfg.CacheVolume, cfg.Instance, logger)
	sched := scheduler.New(logger)
	notifier, err := notify.New(cfg.NotifyURLs, cfg.NotifyTitleTemplate, cfg.NotifyTemplate)
	if err != nil {
		return err
	}
	rec := reconcile.New(ctx, disc, orch, cfg, sched, m, notifier, logger)
	return rec.RunAll(ctx, cCtx.String("app"))
}
