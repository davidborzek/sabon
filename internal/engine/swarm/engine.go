package swarm

// Swarm engine: movers run as one-shot Swarm services pinned to the node that
// holds the data. Run history is the retained
// mover services, reaped down to SABON_MOVER_HISTORY per app/target/action.
//
// NOTE: this path is exercised by unit tests against a fake Swarm client and by
// the swarm e2e in CI; it is NOT validated here against a live cluster.

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/davidborzek/sabon/internal/engine"
	"github.com/davidborzek/sabon/internal/mover"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/pkg/stdcopy"
)

var _ engine.Engine = (*Engine)(nil)

// swarmClient is the subset of the Docker API the swarm runtime drives —
// services, tasks, nodes and the event stream. The real *client.Client
// satisfies it; tests use a fake. Engine, Quiescer, Hooks and Discoverer share
// it: a few methods are unused by any single consumer, but one narrow client
// interface is simpler than several overlapping ones.
type swarmClient interface {
	ServiceCreate(ctx context.Context, service swarm.ServiceSpec, options swarm.ServiceCreateOptions) (swarm.ServiceCreateResponse, error)
	ServiceList(ctx context.Context, options swarm.ServiceListOptions) ([]swarm.Service, error)
	ServiceInspectWithRaw(ctx context.Context, serviceID string, opts swarm.ServiceInspectOptions) (swarm.Service, []byte, error)
	ServiceUpdate(ctx context.Context, serviceID string, version swarm.Version, service swarm.ServiceSpec, options swarm.ServiceUpdateOptions) (swarm.ServiceUpdateResponse, error)
	ServiceRemove(ctx context.Context, serviceID string) error
	ServiceLogs(ctx context.Context, serviceID string, options container.LogsOptions) (io.ReadCloser, error)
	TaskList(ctx context.Context, options swarm.TaskListOptions) ([]swarm.Task, error)
	NodeList(ctx context.Context, options swarm.NodeListOptions) ([]swarm.Node, error)
	Events(ctx context.Context, options events.ListOptions) (<-chan events.Message, <-chan error)
}

// Engine implements engine.Engine by running each mover as a one-shot service.
type Engine struct {
	cli  swarmClient
	poll time.Duration // task-state poll interval
}

// New returns the swarm engine.
func New(cli swarmClient) *Engine {
	return &Engine{cli: cli, poll: time.Second}
}

// RunMover creates a one-shot service for the mover, waits for its task to
// finish, and returns the parsed result. A retained mover's service is left in
// place as history; an ephemeral one is removed.
func (s *Engine) RunMover(ctx context.Context, req mover.Request) (mover.Result, bool, error) {
	spec, err := s.serviceSpec(req)
	if err != nil {
		return mover.Result{}, false, err
	}
	// Swarm won't auto-create a missing bind source the way a standalone daemon
	// does, so a not-yet-existing per-app repo dir gets the task rejected. The
	// only writable bind is the local repo (sources are read-only; remote targets
	// carry no local bind), and a local target is single-node — so the
	// orchestrator's filesystem is the mover's node. Create it.
	for _, m := range spec.TaskTemplate.ContainerSpec.Mounts {
		if m.Type == mount.TypeBind && !m.ReadOnly {
			if err := os.MkdirAll(m.Source, 0o755); err != nil {
				return mover.Result{}, false, fmt.Errorf("create repo dir %q: %w", m.Source, err)
			}
		}
	}
	created, err := s.cli.ServiceCreate(ctx, spec, swarm.ServiceCreateOptions{})
	if err != nil {
		return mover.Result{}, false, fmt.Errorf("create mover service: %w", err)
	}
	id := created.ID

	keep := false
	defer func() {
		if keep {
			return
		}
		_ = s.cli.ServiceRemove(context.WithoutCancel(ctx), id)
	}()

	code, waitErr := s.wait(ctx, id, req.KeepOnShutdown, &keep)
	if waitErr != nil {
		return mover.Result{}, keep, waitErr
	}
	// The task reached a terminal state; retain a terminated mover as run
	// history if asked (reap trims per group). An ephemeral read mover, or one
	// still running after a timeout/cancel, is removed by the defer.
	if req.Retain {
		keep = true
	}

	// Live-output modes (snapshots/check/prune/restore) stream the finished
	// mover's logs to the sink once. NOT `--follow`: `docker service logs
	// --follow` does not EOF when a one-shot service's task completes while the
	// service still lingers, so following would hang the caller (and hold the
	// per-repo lock).
	if req.Stdout != nil {
		s.writeLogs(ctx, id, req.Stdout)
		if code != 0 {
			return mover.Result{}, false, fmt.Errorf("mover task exited with code %d", code)
		}
		return mover.Result{}, false, nil
	}

	logs := s.logs(ctx, id)
	res, _ := mover.ParseResult(logs)
	if code != 0 {
		return res, false, fmt.Errorf("mover task exited with code %d:\n%s", code, tailStr(logs, 2000))
	}
	return res, false, nil
}

// wait polls the service's task until it reaches a terminal state, returning its
// exit code. On ctx cancel it honours KeepOnShutdown (sets *keep).
func (s *Engine) wait(ctx context.Context, serviceID string, keepOnShutdown bool, keep *bool) (int, error) {
	t := time.NewTicker(s.poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if context.Cause(ctx) != context.DeadlineExceeded && keepOnShutdown {
				*keep = true
			}
			return 0, context.Cause(ctx)
		case <-t.C:
			tasks, err := s.cli.TaskList(ctx, swarm.TaskListOptions{Filters: filters.NewArgs(filters.Arg("service", serviceID))})
			if err != nil {
				continue
			}
			for _, tk := range tasks {
				switch tk.Status.State {
				case swarm.TaskStateComplete:
					return exitCode(tk), nil
				case swarm.TaskStateFailed, swarm.TaskStateRejected, swarm.TaskStateShutdown, swarm.TaskStateOrphaned:
					c := exitCode(tk)
					if c == 0 {
						c = 1 // terminal-but-not-complete without a code is still a failure
					}
					return c, nil
				}
			}
		}
	}
}

func (s *Engine) Reap(ctx context.Context, keep int) (int, error) {
	services, err := s.cli.ServiceList(ctx, swarm.ServiceListOptions{Filters: filters.NewArgs(
		filters.Arg("label", mover.LabelKey),
	)})
	if err != nil {
		return 0, err
	}
	// Group terminated history movers by app/target/action and keep the newest
	// N. Action-less services (ephemeral read movers left by a crash) are
	// strays, removed once terminated. Running movers are spared.
	groups := map[string][]swarm.Service{}
	var stray []swarm.Service
	for _, svc := range services {
		if s.serviceRunning(ctx, svc.ID) {
			continue
		}
		l := svc.Spec.Labels
		if l[mover.LabelAction] == "" {
			stray = append(stray, svc)
			continue
		}
		key := l[mover.LabelApp] + "|" + l[mover.LabelTarget] + "|" + l[mover.LabelAction]
		groups[key] = append(groups[key], svc)
	}
	n := 0
	remove := func(id string) {
		if err := s.cli.ServiceRemove(ctx, id); err == nil {
			n++
		}
	}
	for _, svc := range stray {
		remove(svc.ID)
	}
	for _, g := range groups {
		sort.Slice(g, func(i, j int) bool { return g[i].CreatedAt.After(g[j].CreatedAt) })
		for i, svc := range g {
			if i >= keep {
				remove(svc.ID)
			}
		}
	}
	return n, nil
}

func (s *Engine) ListRuns(ctx context.Context, app, target string) ([]engine.RunInfo, error) {
	args := filters.NewArgs(filters.Arg("label", mover.LabelKey), filters.Arg("label", mover.LabelAction))
	if app != "" {
		args.Add("label", mover.LabelApp+"="+app)
	}
	if target != "" {
		args.Add("label", mover.LabelTarget+"="+target)
	}
	services, err := s.cli.ServiceList(ctx, swarm.ServiceListOptions{Filters: args})
	if err != nil {
		return nil, err
	}
	runs := make([]engine.RunInfo, 0, len(services))
	for _, svc := range services {
		runs = append(runs, s.runInfo(ctx, svc))
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].StartedAt.After(runs[j].StartedAt) })
	return runs, nil
}

func (s *Engine) GetRun(ctx context.Context, id string) (engine.RunInfo, bool, error) {
	svc, _, err := s.cli.ServiceInspectWithRaw(ctx, id, swarm.ServiceInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return engine.RunInfo{}, false, nil
	}
	if err != nil {
		return engine.RunInfo{}, false, err
	}
	return s.runInfo(ctx, svc), true, nil
}

func (s *Engine) RunLogs(ctx context.Context, id string, out io.Writer) error {
	rc, err := s.cli.ServiceLogs(ctx, id, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	_, err = stdcopy.StdCopy(out, out, rc)
	return err
}

// ── helpers ──────────────────────────────────────────────────────────────────

func (s *Engine) serviceSpec(req mover.Request) (swarm.ServiceSpec, error) {
	specJSON, err := mover.MarshalSpec(req.Spec)
	if err != nil {
		return swarm.ServiceSpec{}, err
	}
	labels := map[string]string{mover.LabelKey: req.Spec.App}
	for k, v := range req.Labels {
		labels[k] = v
	}
	mounts := append([]mount.Mount{}, req.Mounts...)
	for _, b := range req.Binds {
		if m, ok := bindMount(b); ok {
			mounts = append(mounts, m)
		}
	}
	var placement *swarm.Placement
	if req.Node != "" {
		placement = &swarm.Placement{Constraints: []string{"node.hostname==" + req.Node}}
	}
	one := uint64(1)
	return swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: req.Name, Labels: labels},
		Mode:        swarm.ServiceMode{ReplicatedJob: &swarm.ReplicatedJob{TotalCompletions: &one}},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image:   req.Image,
				Command: []string{"/sabon", "mover"},
				Env:     append([]string{mover.SpecEnv + "=" + specJSON}, req.Env...),
				User:    req.User,
				Groups:  req.Groups,
				Mounts:  mounts,
				Labels:  labels,
				// A mover serves no health endpoint, so the image's HEALTHCHECK must not apply.
				Healthcheck: &container.HealthConfig{Test: []string{"NONE"}},
			},
			RestartPolicy: &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionNone},
			Placement:     placement,
		},
	}, nil
}

func (s *Engine) serviceRunning(ctx context.Context, serviceID string) bool {
	tasks, err := s.cli.TaskList(ctx, swarm.TaskListOptions{Filters: filters.NewArgs(filters.Arg("service", serviceID))})
	if err != nil {
		return true // fail safe: don't reap what we can't inspect
	}
	for _, tk := range tasks {
		switch tk.Status.State {
		case swarm.TaskStateComplete, swarm.TaskStateFailed, swarm.TaskStateRejected, swarm.TaskStateShutdown, swarm.TaskStateOrphaned:
		default:
			return true
		}
	}
	return false
}

func (s *Engine) runInfo(ctx context.Context, svc swarm.Service) engine.RunInfo {
	l := svc.Spec.Labels
	ri := engine.RunInfo{ID: svc.ID, App: l[mover.LabelApp], Target: l[mover.LabelTarget], Action: l[mover.LabelAction], Status: "running"}
	tasks, err := s.cli.TaskList(ctx, swarm.TaskListOptions{Filters: filters.NewArgs(filters.Arg("service", svc.ID))})
	if err != nil || len(tasks) == 0 {
		return ri
	}
	tk := tasks[0]
	for _, t := range tasks[1:] {
		if t.CreatedAt.After(tk.CreatedAt) {
			tk = t
		}
	}
	ri.StartedAt = tk.CreatedAt
	ri.ExitCode = exitCode(tk)
	switch tk.Status.State {
	case swarm.TaskStateComplete:
		ri.Status, ri.FinishedAt = "succeeded", tk.Status.Timestamp
	case swarm.TaskStateFailed, swarm.TaskStateRejected, swarm.TaskStateShutdown, swarm.TaskStateOrphaned:
		ri.Status, ri.FinishedAt = "failed", tk.Status.Timestamp
		if ri.ExitCode == 0 {
			ri.ExitCode = 1
		}
	}
	return ri
}

func (s *Engine) writeLogs(ctx context.Context, id string, out io.Writer) {
	rc, err := s.cli.ServiceLogs(ctx, id, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return
	}
	defer func() { _ = rc.Close() }()
	_, _ = stdcopy.StdCopy(out, out, rc)
}

func (s *Engine) logs(ctx context.Context, id string) string {
	var b strings.Builder
	s.writeLogs(ctx, id, &b)
	return b.String()
}

func exitCode(tk swarm.Task) int {
	if tk.Status.ContainerStatus != nil {
		return tk.Status.ContainerStatus.ExitCode
	}
	return 0
}

func bindMount(b string) (mount.Mount, bool) {
	parts := strings.Split(b, ":")
	if len(parts) < 2 {
		return mount.Mount{}, false
	}
	m := mount.Mount{Type: mount.TypeBind, Source: parts[0], Target: parts[1]}
	if len(parts) >= 3 && parts[2] == "ro" {
		m.ReadOnly = true
	}
	return m, true
}

func tailStr(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
