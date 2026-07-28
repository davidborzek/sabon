// Package docker is the standalone (single-host) runtime: movers are plain
// containers on the local daemon, discovery lists sabon-labelled containers,
// and quiescing stops/starts the app container for a cold backup.
package docker

import (
	"context"
	"io"
	"sort"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/davidborzek/sabon/internal/engine"
	"github.com/davidborzek/sabon/internal/mover"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

var _ engine.Engine = (*Engine)(nil)

// Engine is the standalone engine: movers are plain containers spawned on the
// local daemon, and run history is read from the retained ones.
type Engine struct {
	cli    client.APIClient
	runner *mover.Runner
}

// New returns the standalone engine backed by the given Docker client.
func New(cli client.APIClient) *Engine {
	return &Engine{cli: cli, runner: mover.NewRunner(cli)}
}

func (d *Engine) RunMover(ctx context.Context, req mover.Request) (mover.Result, bool, error) {
	return d.runner.Run(ctx, req)
}

func (d *Engine) Reap(ctx context.Context, keep int) (int, error) {
	return d.runner.Reap(ctx, keep)
}

// ListRuns returns retained mover runs, newest first, optionally filtered by app
// and/or target. Only history movers (those carrying an action label) are
// listed; ephemeral read movers are excluded.
func (d *Engine) ListRuns(ctx context.Context, app, target string) ([]engine.RunInfo, error) {
	list, err := d.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: filtersForRuns(app, target)})
	if err != nil {
		return nil, err
	}
	runs := make([]engine.RunInfo, 0, len(list))
	for _, c := range list {
		ri, err := d.inspectRun(ctx, c.ID)
		if err != nil {
			continue
		}
		runs = append(runs, ri)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].StartedAt.After(runs[j].StartedAt) })
	return runs, nil
}

// GetRun returns one run by id (its mover container id). ok is false when no
// such container exists.
func (d *Engine) GetRun(ctx context.Context, id string) (engine.RunInfo, bool, error) {
	ri, err := d.inspectRun(ctx, id)
	if cerrdefs.IsNotFound(err) {
		return engine.RunInfo{}, false, nil
	}
	if err != nil {
		return engine.RunInfo{}, false, err
	}
	return ri, true, nil
}

// RunLogs writes a run's mover logs (the restic output) to out.
func (d *Engine) RunLogs(ctx context.Context, id string, out io.Writer) error {
	rc, err := d.cli.ContainerLogs(ctx, id, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	_, err = stdcopy.StdCopy(out, out, rc)
	return err
}

func (d *Engine) inspectRun(ctx context.Context, id string) (engine.RunInfo, error) {
	insp, err := d.cli.ContainerInspect(ctx, id)
	if err != nil {
		return engine.RunInfo{}, err
	}
	ri := engine.RunInfo{ID: insp.ID}
	if insp.Config != nil {
		ri.App = insp.Config.Labels[mover.LabelApp]
		ri.Target = insp.Config.Labels[mover.LabelTarget]
		ri.Action = insp.Config.Labels[mover.LabelAction]
	}
	if insp.State != nil {
		ri.ExitCode = insp.State.ExitCode
		switch {
		case insp.State.Running:
			ri.Status = "running"
		case insp.State.ExitCode == 0:
			ri.Status = "succeeded"
		default:
			ri.Status = "failed"
		}
		ri.StartedAt, _ = time.Parse(time.RFC3339Nano, insp.State.StartedAt)
		if !insp.State.Running {
			ri.FinishedAt, _ = time.Parse(time.RFC3339Nano, insp.State.FinishedAt)
		}
	}
	return ri, nil
}

// filtersForRuns matches retained history movers, optionally scoped to an app
// and/or target.
func filtersForRuns(app, target string) filters.Args {
	args := filters.NewArgs(
		filters.Arg("label", mover.LabelKey),
		filters.Arg("label", mover.LabelAction),
	)
	if app != "" {
		args.Add("label", mover.LabelApp+"="+app)
	}
	if target != "" {
		args.Add("label", mover.LabelTarget+"="+target)
	}
	return args
}
