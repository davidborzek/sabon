package backup

import (
	"context"
	"io"
	"sort"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/davidborzek/sabon/internal/mover"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/pkg/stdcopy"
)

// RunInfo is a past or in-flight mover run, reconstructed from its retained
// container so run history survives sabon restarts. The container id is the run
// id, and the container's logs are the restic output.
type RunInfo struct {
	ID         string    `json:"id"`
	App        string    `json:"app"`
	Target     string    `json:"target"`
	Action     string    `json:"action"` // backup | check | prune | restore
	Status     string    `json:"status"` // running | succeeded | failed
	ExitCode   int       `json:"exit_code"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

// ListRuns returns retained mover runs, newest first, optionally filtered by app
// and/or target. Only history movers (those carrying an action label) are
// listed; ephemeral read movers are excluded.
func (o *Orchestrator) ListRuns(ctx context.Context, app, target string) ([]RunInfo, error) {
	args := filtersForRuns(app, target)
	list, err := o.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		return nil, err
	}
	runs := make([]RunInfo, 0, len(list))
	for _, c := range list {
		ri, err := o.inspectRun(ctx, c.ID)
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
func (o *Orchestrator) GetRun(ctx context.Context, id string) (RunInfo, bool, error) {
	ri, err := o.inspectRun(ctx, id)
	if cerrdefs.IsNotFound(err) {
		return RunInfo{}, false, nil
	}
	if err != nil {
		return RunInfo{}, false, err
	}
	return ri, true, nil
}

// RunLogs writes a run's mover logs (the restic output) to out.
func (o *Orchestrator) RunLogs(ctx context.Context, id string, out io.Writer) error {
	rc, err := o.cli.ContainerLogs(ctx, id, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	_, err = stdcopy.StdCopy(out, out, rc)
	return err
}

func (o *Orchestrator) inspectRun(ctx context.Context, id string) (RunInfo, error) {
	insp, err := o.cli.ContainerInspect(ctx, id)
	if err != nil {
		return RunInfo{}, err
	}
	ri := RunInfo{ID: insp.ID}
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
