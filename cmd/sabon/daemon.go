package main

import (
	"context"
	"log/slog"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/davidborzek/sabon/internal/config"
	"github.com/davidborzek/sabon/internal/metrics"
	"github.com/davidborzek/sabon/internal/notify"
	"github.com/davidborzek/sabon/internal/reconcile"
	remoteapi "github.com/davidborzek/sabon/internal/remote/api/v1"
	"github.com/davidborzek/sabon/internal/scheduler"
	"github.com/urfave/cli/v3"
)

// runDaemon is the default action: discover, schedule, and watch for changes.
func runDaemon(ctx context.Context, cmd *cli.Command) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := newLogger(cfg.LogLevel)
	logger.Info("starting sabon", "version", version, "label_prefix", cfg.LabelPrefix, "targets", len(cfg.Targets))

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cli, err := dockerClient()
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()

	m := metrics.New(version)
	m.Serve(ctx, cfg.MetricsAddr, logger)

	image, err := resolveImage(ctx, cli, cfg)
	if err != nil {
		return err
	}
	logger.Info("mover image resolved", "image", image)

	rt, err := newRuntime(ctx, cli, cfg, logger)
	if err != nil {
		return err
	}

	orch := newOrchestrator(cfg, image, rt, logger)
	if n, err := orch.Reap(ctx); err != nil {
		logger.Warn("reap orphan movers failed", "error", err)
	} else if n > 0 {
		logger.Info("reaped orphan movers", "count", n)
	}
	if n, err := orch.ReapSnapshotter(ctx); err != nil {
		logger.Warn("reap orphan snapshots failed", "error", err)
	} else if n > 0 {
		logger.Info("reaped orphan snapshots", "count", n)
	}
	rt.recoverQuiesced(ctx, logger)

	disc := rt.disc
	sched := scheduler.New(logger)
	sched.Start()
	defer sched.Stop()

	notifier, err := notify.New(cfg.NotifyURLs, cfg.NotifyTitleTemplate, cfg.NotifyTemplate)
	if err != nil {
		return err
	}
	rec := reconcile.New(ctx, disc, orch, cfg, sched, m, notifier, logger)
	if err := rec.Reconcile(); err != nil {
		logger.Error("initial reconcile failed", "error", err)
	}
	if cfg.APIAddr != "" {
		remoteapi.New(rec, cfg.APIToken, logger).Serve(ctx, cfg.APIAddr)
	}
	var startupWG sync.WaitGroup
	if cfg.RunOnStartup {
		startupWG.Add(1)
		go func() {
			defer startupWG.Done()
			if err := rec.RunAll(ctx, ""); err != nil {
				logger.Error("run-on-startup backups failed", "error", err)
			}
		}()
	}

	changes := disc.Watch(ctx, m.ObserveWatchRestart)
	ticker := time.NewTicker(cfg.ResyncInterval)
	defer ticker.Stop()
	var debounce <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			logger.Info("shutting down")
			// Let an in-flight run-on-startup backup unwind (its cold-stop restart
			// runs as ctx cancels) so we do not exit leaving an app stopped.
			waitBounded(&startupWG, 30*time.Second, logger)
			return nil
		case <-changes:
			debounce = time.After(cfg.DebounceDelay)
		case <-debounce:
			debounce = nil
			reconcileOnce(rec, logger)
		case <-ticker.C:
			reconcileOnce(rec, logger)
			if n, err := orch.Reap(ctx); err == nil && n > 0 {
				logger.Info("reaped exited movers", "count", n)
			}
		}
	}
}

func reconcileOnce(rec *reconcile.Reconciler, logger *slog.Logger) {
	if err := rec.Reconcile(); err != nil {
		logger.Error("reconcile failed", "error", err)
	}
}

// waitBounded waits for wg up to d, warning if it does not finish in time.
func waitBounded(wg *sync.WaitGroup, d time.Duration, log *slog.Logger) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		log.Warn("run-on-startup backup did not finish within shutdown grace period")
	}
}
