package backup

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/davidborzek/sabon/api"
	"github.com/davidborzek/sabon/internal/discovery"
	"github.com/davidborzek/sabon/internal/engine"
)

func (o *Orchestrator) runHooks(ctx context.Context, job discovery.Job, hooks []api.Hook, phase string, log *slog.Logger) error {
	for i, h := range hooks {
		if err := o.runOneHook(ctx, job, h, phase, i, log); err != nil {
			return err
		}
	}
	return nil
}

func (o *Orchestrator) runOneHook(ctx context.Context, job discovery.Job, h api.Hook, phase string, i int, log *slog.Logger) error {
	d, err := h.TimeoutDuration()
	if err != nil {
		return fmt.Errorf("%s-hook %d: invalid timeout %q: %w", phase, i, h.Timeout, err)
	}
	if d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	// Resolve ${NAME} in hook env from the labelled container's own environment
	// first (the label author owns it), then the SABON_HOOK_ENV_ allowlist.
	var containerEnv map[string]string
	if len(h.Env) > 0 {
		containerEnv, _ = o.hooks.AppEnv(ctx, job.Container)
	}
	env := expandEnv(h.Env, containerEnv)

	if h.Mode() == "run" {
		log.Info("running "+phase+"-hook (run)", "index", i, "image", h.Image)
		spec := engine.RunSpec{
			Name:    hookName(job.App, phase, i),
			Image:   h.Image,
			Command: h.Command,
			User:    h.User,
			Env:     env,
			Network: h.Network,
			Volumes: h.Volumes,
		}
		if err := o.hooks.RunOneShot(ctx, spec); err != nil {
			return fmt.Errorf("%s-hook %d: %w", phase, i, err)
		}
		return nil
	}

	ex, ok := o.hooks.(engine.Execer)
	if !ok {
		return fmt.Errorf("%s-hook %d: exec-mode hooks are not supported by this runtime; use a run-mode hook", phase, i)
	}
	target := h.Container
	if target == "" || target == job.Container {
		target = job.Container
	} else {
		// Cross-container exec is confined to the labelled container's own Compose
		// project, so a label cannot exec a command in an unrelated app's container.
		if job.Project == "" {
			return fmt.Errorf("%s-hook %d: exec target %q is not the labelled container and there is no compose project to scope to", phase, i, target)
		}
		proj, err := o.hooks.AppProject(ctx, target)
		if err != nil {
			return fmt.Errorf("%s-hook %d: inspect exec target %q: %w", phase, i, target, err)
		}
		if proj != job.Project {
			return fmt.Errorf("%s-hook %d: exec target %q is not in compose project %q", phase, i, target, job.Project)
		}
	}
	log.Info("running "+phase+"-hook (exec)", "index", i, "container", target)
	if err := ex.Exec(ctx, target, h.Command, env, h.User); err != nil {
		return fmt.Errorf("%s-hook %d: %w", phase, i, err)
	}
	return nil
}

func hookName(app, phase string, i int) string {
	return fmt.Sprintf("sabon-hook-%s-%s-%d-%d", sanitize(app), phase, i, time.Now().UnixNano())
}
