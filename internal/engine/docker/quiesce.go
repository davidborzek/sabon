package docker

// Standalone quiescing: a cold (offline) backup or in-place restore stops the
// app container and restores its original running state afterwards.

import (
	"context"
	"fmt"
	"sync"

	"github.com/davidborzek/sabon/internal/engine"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

var _ engine.Quiescer = (*Quiescer)(nil)

// Quiescer preserves the app container's original running state in memory.
type Quiescer struct {
	cli     client.APIClient
	mu      sync.Mutex
	stopped map[string]containerState
}

type containerState struct {
	id      string
	running bool
}

// NewQuiescer returns a container quiescer.
func NewQuiescer(cli client.APIClient) *Quiescer { return &Quiescer{cli: cli} }

// Stop stops the container (used for cold backups).
func (q *Quiescer) Stop(ctx context.Context, ref string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	state, ok := q.stopped[ref]
	if !ok {
		info, err := q.cli.ContainerInspect(ctx, ref)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", ref, err)
		}
		if info.ContainerJSONBase == nil || info.ID == "" || info.State == nil {
			return fmt.Errorf("inspect %s: missing container identity or state", ref)
		}
		state = containerState{id: info.ID, running: info.State.Running}
		if q.stopped == nil {
			q.stopped = make(map[string]containerState)
		}
		// Retain the original state even on an ambiguous stop error: a retry
		// must not record our own stopped state or resolve a replacement name.
		q.stopped[ref] = state
	}
	if err := q.cli.ContainerStop(ctx, state.id, container.StopOptions{}); err != nil {
		return fmt.Errorf("stop %s: %w", ref, err)
	}
	return nil
}

// Start restores the original state by immutable ID. Unpaired calls are no-ops.
// Callers coordinate overlapping operations; this state lasts only in-process.
func (q *Quiescer) Start(ctx context.Context, ref string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	state, ok := q.stopped[ref]
	if !ok {
		return nil
	}
	if state.running {
		if err := q.cli.ContainerStart(ctx, state.id, container.StartOptions{}); err != nil {
			return fmt.Errorf("start %s (%s): %w", ref, state.id, err)
		}
	}
	delete(q.stopped, ref)
	return nil
}
