package swarm

// Swarm quiescer: cold backups stop an app by scaling its service to zero and
// restart it by restoring the previous replica count. That count is persisted
// as a label ON THE SERVICE (durable in the swarm store), not in sabon's
// memory — so a crash between Stop and Start cannot strand the service at zero
// replicas. NOT validated against a live cluster here (CI e2e is the gate).

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/davidborzek/sabon/internal/engine"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
)

var _ engine.Quiescer = (*Quiescer)(nil)
var _ engine.Recoverer = (*Quiescer)(nil)

// quiesceReplicasLabel records a service's pre-quiesce replica count on the
// service itself. It survives an orchestrator restart, so Start (or the
// startup RecoverQuiesced sweep) can always bring the app back up.
const quiesceReplicasLabel = "sabon.quiesce.replicas"

// quiesceInstanceLabel records which sabon instance quiesced the service, so a
// startup RecoverQuiesced sweep only revives services this instance owns.
const quiesceInstanceLabel = "sabon.quiesce.instance"

// Quiescer scales a replicated service to zero for a cold backup / in-place
// restore and back up afterwards.
type Quiescer struct {
	cli      swarmClient
	instance string        // owning sabon instance ("" = single-instance, no scoping)
	drain    time.Duration // how long to wait for tasks to drain after scaling to 0
}

// NewQuiescer returns a swarm quiescer.
func NewQuiescer(cli swarmClient, instance string) *Quiescer {
	return &Quiescer{cli: cli, instance: instance, drain: 30 * time.Second}
}

// Stop records the current replica count on the service (once) and scales it to
// zero, then waits for its tasks to drain.
func (q *Quiescer) Stop(ctx context.Context, ref string) error {
	svc, _, err := q.cli.ServiceInspectWithRaw(ctx, ref, swarm.ServiceInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect service %q: %w", ref, err)
	}
	if svc.Spec.Mode.Replicated == nil {
		return fmt.Errorf("service %q is not replicated; a cold backup can only scale replicated services to zero", ref)
	}
	spec := svc.Spec
	if spec.Labels == nil {
		spec.Labels = map[string]string{}
	}
	// Record the count only on the first Stop: a retry, or a second overlapping
	// cold backup of the same service, must not overwrite it with the 0 it was
	// already scaled to.
	if _, ok := spec.Labels[quiesceReplicasLabel]; !ok {
		cur := uint64(1)
		if spec.Mode.Replicated.Replicas != nil {
			cur = *spec.Mode.Replicated.Replicas
		}
		spec.Labels[quiesceReplicasLabel] = strconv.FormatUint(cur, 10)
		if q.instance != "" {
			spec.Labels[quiesceInstanceLabel] = q.instance
		}
	}
	if err := q.scale(ctx, ref, svc.Version, spec, 0); err != nil {
		return err
	}
	q.waitDrained(ctx, ref)
	return nil
}

// Start scales a quiesced service back to its recorded replica count.
func (q *Quiescer) Start(ctx context.Context, ref string) error {
	svc, _, err := q.cli.ServiceInspectWithRaw(ctx, ref, swarm.ServiceInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect service %q: %w", ref, err)
	}
	return q.restore(ctx, ref, svc)
}

// RecoverQuiesced brings every service still carrying the quiesce label back to
// its recorded replica count. It is meant to run once at daemon startup: a
// crash between a cold backup's Stop and Start would otherwise leave services
// stranded at zero replicas. Returns how many were restored.
func (q *Quiescer) RecoverQuiesced(ctx context.Context) (int, error) {
	args := filters.NewArgs(filters.Arg("label", quiesceReplicasLabel))
	if q.instance != "" {
		args.Add("label", quiesceInstanceLabel+"="+q.instance)
	}
	svcs, err := q.cli.ServiceList(ctx, swarm.ServiceListOptions{Filters: args})
	if err != nil {
		return 0, fmt.Errorf("list quiesced services: %w", err)
	}
	n := 0
	for _, svc := range svcs {
		if err := q.restore(ctx, svc.ID, svc); err == nil {
			n++
		}
	}
	return n, nil
}

// restore scales a quiesced service back to its recorded count and drops the
// label. A service without the label is left untouched (nothing to restore).
func (q *Quiescer) restore(ctx context.Context, ref string, svc swarm.Service) error {
	spec := svc.Spec
	raw, ok := spec.Labels[quiesceReplicasLabel]
	if !ok {
		return nil // not quiesced by us
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		n = 1 // corrupt label: fail safe by bringing the app back up
	}
	// A legitimately-recorded 0 means the service was already parked before the
	// backup; restore it to 0 (leave it parked) rather than starting it.
	delete(spec.Labels, quiesceReplicasLabel)
	delete(spec.Labels, quiesceInstanceLabel)
	return q.scale(ctx, ref, svc.Version, spec, n)
}

func (q *Quiescer) scale(ctx context.Context, ref string, version swarm.Version, spec swarm.ServiceSpec, n uint64) error {
	spec.Mode = swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &n}}
	if _, err := q.cli.ServiceUpdate(ctx, ref, version, spec, swarm.ServiceUpdateOptions{}); err != nil {
		return fmt.Errorf("scale service %q to %d: %w", ref, n, err)
	}
	return nil
}

// waitDrained blocks (best-effort, bounded) until the service has no running
// tasks, so the backup reads a quiesced volume rather than a live one.
func (q *Quiescer) waitDrained(ctx context.Context, ref string) {
	deadline := time.Now().Add(q.drain)
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		tasks, err := q.cli.TaskList(ctx, swarm.TaskListOptions{Filters: filters.NewArgs(filters.Arg("service", ref))})
		if err == nil && !anyRunning(tasks) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if time.Now().After(deadline) {
				return
			}
		}
	}
}

func anyRunning(tasks []swarm.Task) bool {
	for _, tk := range tasks {
		switch tk.Status.State {
		case swarm.TaskStateComplete, swarm.TaskStateFailed, swarm.TaskStateRejected, swarm.TaskStateShutdown, swarm.TaskStateOrphaned:
		default:
			return true
		}
	}
	return false
}
