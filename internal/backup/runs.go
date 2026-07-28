package backup

import (
	"context"
	"io"

	"github.com/davidborzek/sabon/internal/engine"
)

// Run history is served by the engine (movers are its records). The orchestrator
// stays the facade the reconciler and API talk to.

// ListRuns returns retained mover runs, newest first, optionally filtered.
func (o *Orchestrator) ListRuns(ctx context.Context, app, target string) ([]engine.RunInfo, error) {
	return o.eng.ListRuns(ctx, app, target)
}

// GetRun returns one run by id; ok is false when none exists.
func (o *Orchestrator) GetRun(ctx context.Context, id string) (engine.RunInfo, bool, error) {
	return o.eng.GetRun(ctx, id)
}

// RunLogs writes a run's restic output to out.
func (o *Orchestrator) RunLogs(ctx context.Context, id string, out io.Writer) error {
	return o.eng.RunLogs(ctx, id, out)
}
