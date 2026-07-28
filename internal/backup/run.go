package backup

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/davidborzek/sabon/api"
	"github.com/davidborzek/sabon/internal/discovery"
	"github.com/davidborzek/sabon/internal/mover"
)

// RunTarget backs one app up to one target and returns the mover's result.
func (o *Orchestrator) RunTarget(ctx context.Context, job discovery.Job, target api.Target) (mover.Result, error) {
	// Per-target source view: drop volumes/paths this target excludes.
	job.Sources = o.sourcesForTarget(job, target)
	// Serialise runs against the same repository (target+app).
	unlock := o.locks.lock(target.Name + "|" + job.App)
	defer unlock()

	// Cap global concurrency (after the per-repo lock, so a run blocked on its
	// repo doesn't hold a slot).
	if err := o.acquire(ctx); err != nil {
		return mover.Result{}, err
	}
	defer o.release()

	// Apply the backup deadline (per-app label override, else the global
	// default). Waiting for the lock/slot above is not counted against it.
	timeout := o.cfg.BackupTimeout
	if d, err := job.Spec.BackupTimeout(); err != nil {
		return mover.Result{}, fmt.Errorf("invalid timeout: %w", err)
	} else if d > 0 {
		timeout = d
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	env, err := o.resticEnv(target, job.App)
	if err != nil {
		return mover.Result{}, err
	}
	if err := o.ensureCache(ctx); err != nil {
		return mover.Result{}, err
	}

	log := o.log.With("app", job.App, "target", target.Name)

	// Pre-hooks (e.g. pg_dump), then optional cold-backup stop.
	if err := o.runHooks(ctx, job, job.Spec.PreHooks, "pre", log); err != nil {
		return mover.Result{}, err
	}
	if job.Spec.Stop {
		// Coordinate the stop across concurrent runs of the same container (an
		// app backed up to several targets at once): stop once, start once.
		cs := o.colds.get(job.Container)
		if err := cs.hold(func() error {
			log.Info("stopping container for cold backup", "container", job.Container)
			return o.quiesce.Stop(ctx, job.Container)
		}); err != nil {
			return mover.Result{}, err
		}
		// Always attempt to restart (even on mover failure or shutdown); the last
		// concurrent holder does it.
		defer cs.release(func() {
			if err := o.quiesce.Start(context.WithoutCancel(ctx), job.Container); err != nil {
				log.Error("failed to restart container after cold backup", "container", job.Container, "error", err)
			}
		})
	}

	// Snapshot sources after pre-hooks and any cold stop, so a snapshot captures
	// dumped/quiesced data. "none" yields live source mounts.
	srcMounts, snapCleanup, err := o.sourceMountsFor(ctx, job)
	if err != nil {
		return mover.Result{}, err
	}
	// Destroy the snapshot when the run ends — unless the mover is left running
	// on shutdown (set below): then it must survive for the surviving backup and
	// is cleaned by a later reap once that mover exits.
	keptMover := false
	defer func() {
		if !keptMover {
			snapCleanup(context.WithoutCancel(ctx))
		}
	}()
	cacheMs, binds := o.cacheAndRepo(target, job.App)
	mounts := append(srcMounts, cacheMs...)

	req := mover.Request{
		Name:           moverName(job.App, target.Name),
		Image:          o.image,
		User:           job.Spec.MoverUser(o.cfg.MoverUser),
		Groups:         job.Spec.MoverGroups(o.cfg.MoverGroups),
		Network:        o.cfg.MoverNetwork,
		Spec:           o.moverSpec(job, target),
		Env:            env,
		Mounts:         mounts,
		Binds:          binds,
		KeepOnShutdown: !job.Spec.Stop,
		Labels:         moverLabels(job.App, target.Name, "backup"),
		Retain:         true,
		Node:           job.Node,
	}
	log.Debug("spawning mover", "name", req.Name, "image", req.Image, "user", req.User, "sources", len(srcMounts))
	res, kept, runErr := o.eng.RunMover(ctx, req)
	keptMover = kept

	// Post-hooks run regardless of the mover outcome, and must survive a
	// timeout/shutdown cancel of ctx (they may revert pre-hook state, e.g. take
	// a database out of backup mode), so run them on a fresh bounded context.
	postCtx, postCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer postCancel()
	if err := o.runHooks(postCtx, job, job.Spec.PostHooks, "post", log); err != nil {
		log.Error("post-hook failed", "error", err)
	}
	return res, runErr
}

// RunSnapshots lists the snapshots in an app's repository for a target, writing
// restic's output to out.
func (o *Orchestrator) RunSnapshots(ctx context.Context, app string, target api.Target, out io.Writer) error {
	unlock := o.locks.lock(target.Name + "|" + app)
	defer unlock()

	env, err := o.resticEnv(target, app)
	if err != nil {
		return err
	}
	if err := o.ensureCache(ctx); err != nil {
		return err
	}
	mounts, binds := o.cacheAndRepo(target, app)
	_, _, err = o.eng.RunMover(ctx, mover.Request{
		Name:   moverName(app, target.Name),
		Image:  o.image,
		User:   o.cfg.MoverUser,
		Groups: o.cfg.MoverGroups,
		Spec:   mover.Spec{App: app, Action: mover.ActionSnapshots, ExtraArgs: target.ResticArgs},
		Env:    env,
		Mounts: mounts,
		Binds:  binds,
		Stdout: out,
	})
	return err
}

// RunCheck runs `restic check` (repository integrity) for an app's repository
// in a target. When out is non-nil the restic output is streamed there;
// otherwise it is captured and surfaced only on failure (scheduled runs).
func (o *Orchestrator) RunCheck(ctx context.Context, app string, target api.Target, out io.Writer) error {
	unlock := o.locks.lock(target.Name + "|" + app)
	defer unlock()
	if err := o.acquire(ctx); err != nil {
		return err
	}
	defer o.release()

	env, err := o.resticEnv(target, app)
	if err != nil {
		return err
	}
	if err := o.ensureCache(ctx); err != nil {
		return err
	}
	mounts, binds := o.cacheAndRepo(target, app)
	_, _, err = o.eng.RunMover(ctx, mover.Request{
		Name:   moverName(app, target.Name),
		Image:  o.image,
		User:   o.cfg.MoverUser,
		Groups: o.cfg.MoverGroups,
		Spec:   mover.Spec{App: app, Action: mover.ActionCheck, ExtraArgs: target.ResticArgs},
		Env:    env,
		Mounts: mounts,
		Binds:  binds,
		Stdout: out,
		Labels: moverLabels(app, target.Name, "check"),
		Retain: true,
	})
	return err
}

// RunPrune runs `restic prune` (repack / reclaim space) for an app's repository
// in a target. Same streaming/capture semantics as RunCheck.
func (o *Orchestrator) RunPrune(ctx context.Context, app string, target api.Target, out io.Writer) error {
	unlock := o.locks.lock(target.Name + "|" + app)
	defer unlock()
	if err := o.acquire(ctx); err != nil {
		return err
	}
	defer o.release()

	env, err := o.resticEnv(target, app)
	if err != nil {
		return err
	}
	if err := o.ensureCache(ctx); err != nil {
		return err
	}
	mounts, binds := o.cacheAndRepo(target, app)
	_, _, err = o.eng.RunMover(ctx, mover.Request{
		Name:   moverName(app, target.Name),
		Image:  o.image,
		User:   o.cfg.MoverUser,
		Groups: o.cfg.MoverGroups,
		Spec:   mover.Spec{App: app, Action: mover.ActionPrune, ExtraArgs: target.ResticArgs},
		Env:    env,
		Mounts: mounts,
		Binds:  binds,
		Stdout: out,
		Labels: moverLabels(app, target.Name, "prune"),
		Retain: true,
	})
	return err
}

// RestoreOptions configure a restore run.
type RestoreOptions struct {
	Snapshot string   // snapshot ID; default "latest"
	Into     string   // host dir for a staging restore; empty => in-place
	Stop     bool     // stop the app during an in-place restore
	Include  []string // limit the restore to matching paths
}

// RunRestore restores a snapshot. With opts.Into set it restores into a host
// staging directory (safe; the app need not exist). Otherwise it restores
// in-place into the app's live volumes (job required), optionally stopping the
// app first. restic output is written to out.
func (o *Orchestrator) RunRestore(ctx context.Context, app string, job *discovery.Job, target api.Target, opts RestoreOptions, out io.Writer) error {
	unlock := o.locks.lock(target.Name + "|" + app)
	defer unlock()

	env, err := o.resticEnv(target, app)
	if err != nil {
		return err
	}
	if err := o.ensureCache(ctx); err != nil {
		return err
	}
	mounts, binds := o.cacheAndRepo(target, app)
	spec := mover.Spec{App: app, Action: mover.ActionRestore, SnapshotID: opts.Snapshot, Include: opts.Include, ExtraArgs: target.ResticArgs}

	if opts.Into != "" {
		binds = append(binds, opts.Into+":"+restoreMount)
		spec.RestoreTarget = restoreMount
	} else {
		if job == nil {
			return fmt.Errorf("in-place restore needs the app's live container; use --into <dir> for staging")
		}
		mounts = append(mounts, o.sourceMounts(*job, false)...)
		spec.RestoreTarget = "/"
		if opts.Stop {
			log := o.log.With("app", app, "target", target.Name)
			log.Info("stopping container for in-place restore", "container", job.Container)
			if err := o.quiesce.Stop(ctx, job.Container); err != nil {
				return err
			}
			defer func() {
				if err := o.quiesce.Start(context.WithoutCancel(ctx), job.Container); err != nil {
					log.Error("failed to restart container after restore", "container", job.Container, "error", err)
				}
			}()
		}
	}

	// The per-app user applies to in-place restore (job present), where files
	// are written into the app's live volumes; staging restore uses the default.
	user := o.cfg.MoverUser
	groups := o.cfg.MoverGroups
	var node string
	if job != nil {
		user = job.Spec.MoverUser(o.cfg.MoverUser)
		groups = job.Spec.MoverGroups(o.cfg.MoverGroups)
		node = job.Node
	}

	_, _, err = o.eng.RunMover(ctx, mover.Request{
		Name:   moverName(app, target.Name),
		Image:  o.image,
		User:   user,
		Groups: groups,
		Spec:   spec,
		Env:    env,
		Mounts: mounts,
		Binds:  binds,
		Stdout: out,
		Labels: moverLabels(app, target.Name, "restore"),
		Retain: true,
		Node:   node,
	})
	return err
}

// moverLabels tags a mover with the run-history labels the API reads back.
func moverLabels(app, target, action string) map[string]string {
	return map[string]string{
		mover.LabelApp:    app,
		mover.LabelTarget: target,
		mover.LabelAction: action,
	}
}
