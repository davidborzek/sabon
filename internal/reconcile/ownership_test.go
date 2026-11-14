package reconcile

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/davidborzek/sabon/api"
	"github.com/davidborzek/sabon/internal/config"
	"github.com/davidborzek/sabon/internal/discovery"
	"github.com/davidborzek/sabon/internal/engine"
	"github.com/davidborzek/sabon/internal/metrics"
	"github.com/davidborzek/sabon/internal/scheduler"
)

type ownershipDiscoverer struct {
	engine.Discoverer
	jobs []discovery.Job
	err  error
}

func (d *ownershipDiscoverer) List(context.Context) ([]discovery.Job, error) { return d.jobs, d.err }

func TestOwnershipReconcileAndOneShot(t *testing.T) {
	for _, source := range []string{"discovery", "validation"} {
		t.Run(source, func(t *testing.T) {
			ctx := context.Background()
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			d := &ownershipDiscoverer{jobs: []discovery.Job{{App: "alpha-db", Container: "alpha-db-1"}, {App: "beta-db", Container: "beta-db-1", Backup: "data"}, {App: "unrelated", Container: "other"}}}
			s := scheduler.New(log)
			cfg := &config.Config{Targets: []api.Target{{Name: "local", Schedule: "0 0 * * * *", Check: "0 0 1 * * *", Prune: "0 0 2 * * *"}}}
			r := New(ctx, d, nil, cfg, s, metrics.New("test"), nil, log)
			if err := r.Reconcile(); err != nil || s.Len() != 9 {
				t.Fatalf("distinct owners: schedules=%d err=%v", s.Len(), err)
			}
			d.jobs[1].App = "alpha-db"
			if source == "discovery" {
				d.err = discovery.ValidateOwnership(d.jobs)
			}
			if err := r.Reconcile(); !errors.Is(err, discovery.ErrOwnership) {
				t.Fatalf("want ownership error, got %v", err)
			}
			if s.Len() != 0 {
				t.Fatalf("stale schedules still active: %d", s.Len())
			}
			// A nil orchestrator ensures rejection happens before any execution.
			if err := r.RunAll(ctx, ""); err == nil {
				t.Fatal("RunAll accepted conflicting owners")
			}
			if err := r.RunMatching(ctx, "alpha-db", "local"); err == nil {
				t.Fatal("filtered run accepted conflicting owners")
			}
			if _, _, err := r.resolve(ctx, "alpha-db", "local"); err == nil {
				t.Fatal("API picked an arbitrary owner")
			}
			// Even disjoint target selections cannot make an app-only lookup
			// (including in-place restore) safe.
			d.jobs[0].Spec.Targets = []api.TargetSpec{{Name: "local"}}
			d.jobs[1].Spec.Targets = []api.TargetSpec{{Name: "offsite"}}
			if _, _, err := r.resolve(ctx, "alpha-db", ""); err == nil {
				t.Fatal("API accepted ambiguous app on disjoint targets")
			}
			d.jobs = d.jobs[:1]
			d.err = nil
			if err := r.Reconcile(); err != nil || s.Len() != 3 {
				t.Fatalf("recovery: schedules=%d err=%v", s.Len(), err)
			}
		})
	}
}
