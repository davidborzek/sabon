// Package reconcile bridges discovery and the scheduler: on each reconcile it
// lists backup jobs and updates the cron schedule set, and it executes
// individual backup runs while recording metrics.
package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/davidborzek/sabon/api"
	"github.com/davidborzek/sabon/internal/backup"
	"github.com/davidborzek/sabon/internal/config"
	"github.com/davidborzek/sabon/internal/discovery"
	"github.com/davidborzek/sabon/internal/engine"
	"github.com/davidborzek/sabon/internal/metrics"
	"github.com/davidborzek/sabon/internal/notify"
	"github.com/davidborzek/sabon/internal/scheduler"
)

// Reconciler wires discovery, the orchestrator, the scheduler and metrics.
type Reconciler struct {
	ctx      context.Context // long-lived context for scheduled runs
	disc     engine.Discoverer
	orch     *backup.Orchestrator
	cfg      *config.Config
	sched    *scheduler.Scheduler
	metrics  *metrics.Metrics
	notifier *notify.Notifier
	log      *slog.Logger
}

// New returns a Reconciler. ctx is the daemon context used by scheduled runs.
func New(ctx context.Context, disc engine.Discoverer, orch *backup.Orchestrator, cfg *config.Config, sched *scheduler.Scheduler, m *metrics.Metrics, notifier *notify.Notifier, log *slog.Logger) *Reconciler {
	return &Reconciler{ctx: ctx, disc: disc, orch: orch, cfg: cfg, sched: sched, metrics: m, notifier: notifier, log: log}
}

// plan is a discovered job paired with one resolved target and schedule.
type plan struct {
	job      discovery.Job
	target   api.Target
	schedule string
}

// Reconcile lists jobs and syncs the scheduler to match.
func (r *Reconciler) Reconcile() error {
	jobs, err := r.disc.List(r.ctx)
	if err != nil {
		r.metrics.ObserveReconcile(false)
		return err
	}
	r.metrics.SetManagedJobs(len(jobs))

	var desired []scheduler.Job
	seen := map[string]bool{}
	for _, p := range r.plans(jobs) {
		app, target := p.job.App, p.target
		if p.schedule != "" {
			key := app + "|" + target.Name
			if seen[key] {
				r.log.Warn("duplicate app|target: another container already backs up this repo, skipping the duplicate", "app", app, "target", target.Name)
			} else {
				seen[key] = true
				desired = append(desired, scheduler.Job{
					Key:         key,
					Schedule:    p.schedule,
					Fingerprint: jobFingerprint(p.job, target),
					Run:         r.runFunc(p.job, p.target),
				})
			}
		} else {
			r.log.Warn("no backup schedule for target; runnable via `sabon backup` (check/prune still scheduled if set)", "app", app, "target", target.Name)
		}
		if target.Check != "" {
			desired = append(desired, scheduler.Job{
				Key:      app + "|" + target.Name + "|check",
				Schedule: target.Check,
				Run:      func() { r.jitter(r.ctx); _ = r.runCheck(r.ctx, app, target) },
			})
		}
		if target.Prune != "" {
			desired = append(desired, scheduler.Job{
				Key:      app + "|" + target.Name + "|prune",
				Schedule: target.Prune,
				Run:      func() { r.jitter(r.ctx); _ = r.runPrune(r.ctx, app, target) },
			})
		}
	}
	r.sched.Sync(desired)
	r.log.Debug("reconciled", "jobs", len(jobs), "scheduled", r.sched.Len())
	activeApps := make(map[string]bool, len(jobs))
	for _, j := range jobs {
		activeApps[j.App] = true
	}
	r.metrics.ForgetAbsent(activeApps)
	r.metrics.ObserveReconcile(true)
	return nil
}

// plans expands jobs into (job, target, schedule) tuples.
func (r *Reconciler) plans(jobs []discovery.Job) []plan {
	var out []plan
	for _, job := range jobs {
		for _, t := range r.targetsFor(job) {
			sched := t.Schedule
			if ts, ok := job.Spec.TargetOverride(t.Name); ok && ts.Schedule != "" {
				sched = ts.Schedule
			}
			out = append(out, plan{job: job, target: t, schedule: sched})
		}
	}
	return out
}

// targetsFor resolves the targets a job backs up to.
func (r *Reconciler) targetsFor(job discovery.Job) []api.Target {
	if len(job.Spec.Targets) == 0 {
		return r.cfg.Targets
	}
	var ts []api.Target
	for _, name := range job.Spec.TargetNames() {
		if t, ok := r.cfg.Target(name); ok {
			ts = append(ts, t)
		} else {
			r.log.Warn("unknown target referenced by label", "app", job.App, "target", name)
		}
	}
	return ts
}

func (r *Reconciler) runFunc(job discovery.Job, target api.Target) func() {
	return func() { r.jitter(r.ctx); _ = r.runOnce(r.ctx, job, target) }
}

// jitter sleeps a random 0..SABON_SCHEDULE_JITTER before a scheduled run to
// avoid a thundering herd when many jobs share a cron time.
func (r *Reconciler) jitter(ctx context.Context) {
	if r.cfg.ScheduleJitter <= 0 {
		return
	}
	select {
	case <-time.After(rand.N(r.cfg.ScheduleJitter)):
	case <-ctx.Done():
	}
}

// runOnce executes one backup, records metrics, and notifies.
func (r *Reconciler) runOnce(ctx context.Context, job discovery.Job, target api.Target) error {
	log := r.log.With("app", job.App, "target", target.Name)
	log.Info("backup starting")
	start := time.Now()
	res, err := r.orch.RunTarget(ctx, job, target)
	dur := time.Since(start)
	r.metrics.ObserveBackup(job.App, target.Name, err == nil, dur, res.FilesNew, res.DataAddedBytes)
	if err != nil {
		log.Error("backup failed", "error", err, "duration", dur)
		r.sendNotify(notify.Data{Event: "backup", App: job.App, Target: target.Name, Duration: dur.Round(time.Second), Error: err.Error()})
		return err
	}
	log.Info("backup complete", "duration", dur, "snapshot", res.SnapshotID, "files_new", res.FilesNew, "data_added_bytes", res.DataAddedBytes)
	r.sendNotify(notify.Data{Event: "backup", App: job.App, Target: target.Name, OK: true, Duration: dur.Round(time.Second), SnapshotID: res.SnapshotID, FilesNew: res.FilesNew, DataAdded: res.DataAddedBytes})
	return nil
}

// runCheck runs `restic check` for an app/target, records metrics, and notifies
// on failure.
func (r *Reconciler) runCheck(ctx context.Context, app string, target api.Target) error {
	log := r.log.With("app", app, "target", target.Name)
	log.Info("repository check starting")
	start := time.Now()
	err := r.orch.RunCheck(ctx, app, target, nil)
	dur := time.Since(start)
	r.metrics.ObserveCheck(app, target.Name, err == nil)
	if err != nil {
		log.Error("repository check failed", "error", err, "duration", dur)
		r.sendNotify(notify.Data{Event: "check", App: app, Target: target.Name, Duration: dur.Round(time.Second), Error: err.Error()})
		return err
	}
	log.Info("repository check ok", "duration", dur)
	return nil
}

// runPrune runs `restic prune` for an app/target, records metrics, and notifies
// on failure.
func (r *Reconciler) runPrune(ctx context.Context, app string, target api.Target) error {
	log := r.log.With("app", app, "target", target.Name)
	log.Info("repository prune starting")
	start := time.Now()
	err := r.orch.RunPrune(ctx, app, target, nil)
	dur := time.Since(start)
	r.metrics.ObservePrune(app, target.Name, err == nil)
	if err != nil {
		log.Error("repository prune failed", "error", err, "duration", dur)
		r.sendNotify(notify.Data{Event: "prune", App: app, Target: target.Name, Duration: dur.Round(time.Second), Error: err.Error()})
		return err
	}
	log.Info("repository prune ok", "duration", dur)
	return nil
}

// sendNotify delivers a notification per the configured policy: failures always
// notify; a success notifies only for a backup when SABON_NOTIFY_ON=always.
func (r *Reconciler) sendNotify(d notify.Data) {
	if !r.notifier.Enabled() {
		return
	}
	d.Instance = r.cfg.Instance
	if d.OK && (d.Event != "backup" || r.cfg.NotifyOn != "always") {
		return
	}
	if err := r.notifier.Send(d); err != nil {
		r.log.Warn("notification failed", "app", d.App, "target", d.Target, "event", d.Event, "error", err)
	}
}

// RunAll runs every discovered job against all its targets, synchronously.
// Used by --run-on-startup and the one-shot `sabon backup` command.
func (r *Reconciler) RunAll(ctx context.Context, appFilter string) error {
	jobs, err := r.disc.List(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range r.plans(jobs) {
		if appFilter != "" && p.job.App != appFilter {
			continue
		}
		if err := r.runOnce(ctx, p.job, p.target); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// jobFingerprint captures the parts of a backup job that drive its execution
// (resolved sources plus the spec/target fields the mover reads), so a change
// re-registers the scheduled run even when the cron string is unchanged.
func jobFingerprint(job discovery.Job, target api.Target) string {
	b, _ := json.Marshal(struct {
		Sources []discovery.Source
		Spec    api.Spec
		Target  api.Target
	}{job.Sources, job.Spec, target})
	return string(b)
}

// ── HTTP API surface ─────────────────────────────────────────────────────────
// These expose the same operations as the CLI and scheduler for the optional
// control API (internal/remote/api/v1), reusing target resolution, metrics and
// notifications so an API-triggered run is indistinguishable from a scheduled
// one. The Reconciler satisfies the remote API's Backend (internal/remote/api/v1).

// ConfigTargets returns the configured backup targets.
func (r *Reconciler) ConfigTargets() []api.Target { return r.cfg.Targets }

// Jobs returns the currently discovered backup jobs.
func (r *Reconciler) Jobs(ctx context.Context) ([]discovery.Job, error) {
	return r.disc.List(ctx)
}

// resolve finds the discovered job for app plus the targets to act on: all of
// the app's targets when targetName is empty, else just the named one (which
// must be one the app backs up to).
func (r *Reconciler) resolve(ctx context.Context, app, targetName string) (discovery.Job, []api.Target, error) {
	jobs, err := r.disc.List(ctx)
	if err != nil {
		return discovery.Job{}, nil, err
	}
	var job discovery.Job
	found := false
	for _, j := range jobs {
		if j.App == app {
			job, found = j, true
			break
		}
	}
	if !found {
		return discovery.Job{}, nil, fmt.Errorf("no backup job for app %q", app)
	}
	targets := r.targetsFor(job)
	if targetName == "" {
		return job, targets, nil
	}
	for _, t := range targets {
		if t.Name == targetName {
			return job, []api.Target{t}, nil
		}
	}
	return discovery.Job{}, nil, fmt.Errorf("app %q does not back up to target %q", app, targetName)
}

// Backup runs a backup of app to targetName (empty = all its targets).
func (r *Reconciler) Backup(ctx context.Context, app, targetName string) error {
	job, targets, err := r.resolve(ctx, app, targetName)
	if err != nil {
		return err
	}
	var errs []error
	for _, t := range targets {
		if err := r.runOnce(ctx, job, t); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Check runs a repository check for app on targetName (empty = all its targets).
func (r *Reconciler) Check(ctx context.Context, app, targetName string) error {
	_, targets, err := r.resolve(ctx, app, targetName)
	if err != nil {
		return err
	}
	var errs []error
	for _, t := range targets {
		if err := r.runCheck(ctx, app, t); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Prune runs a repository prune for app on targetName (empty = all its targets).
func (r *Reconciler) Prune(ctx context.Context, app, targetName string) error {
	_, targets, err := r.resolve(ctx, app, targetName)
	if err != nil {
		return err
	}
	var errs []error
	for _, t := range targets {
		if err := r.runPrune(ctx, app, t); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Snapshots lists the restic snapshots for app in targetName, writing restic's
// output to out. targetName is required (a snapshot list is per repository).
func (r *Reconciler) Snapshots(ctx context.Context, app, targetName string, out io.Writer) error {
	if targetName == "" {
		return fmt.Errorf("target is required")
	}
	_, targets, err := r.resolve(ctx, app, targetName)
	if err != nil {
		return err
	}
	return r.orch.RunSnapshots(ctx, app, targets[0], out)
}

// Restore restores app from targetName (required). With opts.Into set it stages
// into a host dir; otherwise it restores in-place into the app's live volumes.
func (r *Reconciler) Restore(ctx context.Context, app, targetName string, opts backup.RestoreOptions, out io.Writer) error {
	if targetName == "" {
		return fmt.Errorf("target is required")
	}
	job, targets, err := r.resolve(ctx, app, targetName)
	if err != nil {
		return err
	}
	var jp *discovery.Job
	if opts.Into == "" {
		jp = &job // in-place needs the live container
	}
	return r.orch.RunRestore(ctx, app, jp, targets[0], opts, out)
}

// ListRuns returns retained mover runs (run history), newest first.
func (r *Reconciler) ListRuns(ctx context.Context, app, target string) ([]engine.RunInfo, error) {
	return r.orch.ListRuns(ctx, app, target)
}

// GetRun returns one run by id (its mover container id).
func (r *Reconciler) GetRun(ctx context.Context, id string) (engine.RunInfo, bool, error) {
	return r.orch.GetRun(ctx, id)
}

// RunLogs writes a run's mover logs (restic output) to out.
func (r *Reconciler) RunLogs(ctx context.Context, id string, out io.Writer) error {
	return r.orch.RunLogs(ctx, id, out)
}
