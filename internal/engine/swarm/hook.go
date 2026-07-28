package swarm

// Swarm hooks: run-mode hooks are one-shot services; env/project are read from
// the service spec; reap removes finished one-shot hook services. Swarm does not
// implement the Execer capability (a manager cannot exec into a task on another
// node), so exec-mode hooks fall back to a runtime error at the orchestrator.
//
// NOTE: unit-tested against a fake swarm client; NOT validated here against a
// live cluster (CI e2e is the gate). Run-mode hooks carry no node constraint,
// so a hook mounting a node-local volume may land on the wrong node — a known
// swarm limitation.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/davidborzek/sabon/internal/engine"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/pkg/stdcopy"
)

var _ engine.Hooks = (*Hooks)(nil)

// Hooks runs pre/post backup actions against a Swarm cluster.
type Hooks struct {
	cli  swarmClient
	poll time.Duration // task-state poll interval
}

// NewHooks returns a swarm hook runner.
func NewHooks(cli swarmClient) *Hooks { return &Hooks{cli: cli, poll: time.Second} }

// RunOneShot spawns a one-shot service from spec.Image, waits for its task to
// finish, and removes the service. Non-zero exit (with logs) is an error.
func (h *Hooks) RunOneShot(ctx context.Context, spec engine.RunSpec) error {
	if spec.Image == "" {
		return fmt.Errorf("hook: run mode needs an image")
	}
	mounts, err := hookMounts(spec.Volumes)
	if err != nil {
		return err
	}
	one := uint64(1)
	sspec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: spec.Name, Labels: map[string]string{engine.HookLabelKey: spec.Name}},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image:   spec.Image,
				Command: spec.Command,
				Env:     spec.Env,
				User:    spec.User,
				Mounts:  mounts,
				Labels:  map[string]string{engine.HookLabelKey: spec.Name},
			},
			RestartPolicy: &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionNone},
		},
		Mode: swarm.ServiceMode{ReplicatedJob: &swarm.ReplicatedJob{TotalCompletions: &one}},
	}
	if spec.Network != "" {
		sspec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: spec.Network}}
	}

	created, err := h.cli.ServiceCreate(ctx, sspec, swarm.ServiceCreateOptions{})
	if err != nil {
		return fmt.Errorf("create hook service: %w", err)
	}
	defer func() { _ = h.cli.ServiceRemove(context.WithoutCancel(ctx), created.ID) }()

	code, err := h.wait(ctx, created.ID)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("hook service %s exited with code %d:\n%s", spec.Image, code, h.serviceLogs(ctx, created.ID))
	}
	return nil
}

// wait polls the hook service's task until it reaches a terminal state and
// returns its exit code.
func (h *Hooks) wait(ctx context.Context, serviceID string) (int, error) {
	t := time.NewTicker(h.poll)
	defer t.Stop()
	for {
		tasks, err := h.cli.TaskList(ctx, swarm.TaskListOptions{Filters: filters.NewArgs(filters.Arg("service", serviceID))})
		if err == nil {
			for _, tk := range tasks {
				switch tk.Status.State {
				case swarm.TaskStateComplete:
					return exitCode(tk), nil
				case swarm.TaskStateFailed, swarm.TaskStateRejected, swarm.TaskStateShutdown, swarm.TaskStateOrphaned:
					if c := exitCode(tk); c != 0 {
						return c, nil
					}
					return 1, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-t.C:
		}
	}
}

// Reap removes finished (no running task) one-shot hook services left behind by
// a crash or an aborted run. Returns how many were removed.
func (h *Hooks) Reap(ctx context.Context) (int, error) {
	svcs, err := h.cli.ServiceList(ctx, swarm.ServiceListOptions{Filters: filters.NewArgs(filters.Arg("label", engine.HookLabelKey))})
	if err != nil {
		return 0, fmt.Errorf("list hook services: %w", err)
	}
	n := 0
	for _, svc := range svcs {
		if h.running(ctx, svc.ID) {
			continue
		}
		if err := h.cli.ServiceRemove(ctx, svc.ID); err == nil {
			n++
		}
	}
	return n, nil
}

func (h *Hooks) running(ctx context.Context, serviceID string) bool {
	tasks, err := h.cli.TaskList(ctx, swarm.TaskListOptions{Filters: filters.NewArgs(filters.Arg("service", serviceID))})
	if err != nil {
		return true // fail safe: don't reap what we can't inspect
	}
	return anyRunning(tasks)
}

// AppEnv returns the app service's environment as a KEY->VALUE map.
func (h *Hooks) AppEnv(ctx context.Context, id string) (map[string]string, error) {
	svc, _, err := h.cli.ServiceInspectWithRaw(ctx, id, swarm.ServiceInspectOptions{})
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	cs := svc.Spec.TaskTemplate.ContainerSpec
	if cs == nil {
		return out, nil
	}
	for _, kv := range cs.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out, nil
}

// AppProject returns the app service's Docker Compose project label, or "".
func (h *Hooks) AppProject(ctx context.Context, id string) (string, error) {
	svc, _, err := h.cli.ServiceInspectWithRaw(ctx, id, swarm.ServiceInspectOptions{})
	if err != nil {
		return "", err
	}
	return svc.Spec.Labels["com.docker.compose.project"], nil
}

func (h *Hooks) serviceLogs(ctx context.Context, id string) string {
	rc, err := h.cli.ServiceLogs(ctx, id, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return ""
	}
	defer func() { _ = rc.Close() }()
	var b strings.Builder
	_, _ = stdcopy.StdCopy(&b, &b, rc)
	return b.String()
}

// hookMounts turns "source:/target[:ro]" strings into swarm mounts. A source
// beginning with "/" is a bind mount; otherwise it is a named volume.
func hookMounts(vols []string) ([]mount.Mount, error) {
	var ms []mount.Mount
	for _, v := range vols {
		parts := strings.Split(v, ":")
		if len(parts) < 2 || len(parts) > 3 {
			return nil, fmt.Errorf("hook volume %q: want source:/target[:ro]", v)
		}
		m := mount.Mount{Source: parts[0], Target: parts[1]}
		if strings.HasPrefix(parts[0], "/") {
			m.Type = mount.TypeBind
		} else {
			m.Type = mount.TypeVolume
		}
		if len(parts) == 3 {
			if parts[2] != "ro" {
				return nil, fmt.Errorf("hook volume %q: third field must be 'ro'", v)
			}
			m.ReadOnly = true
		}
		ms = append(ms, m)
	}
	return ms, nil
}
