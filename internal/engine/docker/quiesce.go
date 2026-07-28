package docker

// Standalone quiescing: a cold (offline) backup or in-place restore stops the
// app container and starts it again afterwards.

import (
	"context"
	"fmt"

	"github.com/davidborzek/sabon/internal/engine"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

var _ engine.Quiescer = (*Quiescer)(nil)

// Quiescer stops and starts the app container.
type Quiescer struct {
	cli client.APIClient
}

// NewQuiescer returns a container quiescer.
func NewQuiescer(cli client.APIClient) *Quiescer { return &Quiescer{cli: cli} }

// Stop stops the container (used for cold backups).
func (q *Quiescer) Stop(ctx context.Context, ref string) error {
	if err := q.cli.ContainerStop(ctx, ref, container.StopOptions{}); err != nil {
		return fmt.Errorf("stop %s: %w", ref, err)
	}
	return nil
}

// Start (re)starts the container.
func (q *Quiescer) Start(ctx context.Context, ref string) error {
	if err := q.cli.ContainerStart(ctx, ref, container.StartOptions{}); err != nil {
		return fmt.Errorf("start %s: %w", ref, err)
	}
	return nil
}
