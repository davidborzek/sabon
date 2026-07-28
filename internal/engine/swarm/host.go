package swarm

// Host is the swarm no-op engine.Host: snapshots are unsupported and movers are
// node-pinned services, so there is no manager-local host/volume access to
// provide. A node's local volumes are not resolvable from the manager anyway.

import (
	"context"

	"github.com/davidborzek/sabon/internal/engine"
)

var _ engine.Host = (*Host)(nil)

// Host is the swarm host accessor (no-op).
type Host struct{}

// NewHost returns the swarm no-op host accessor.
func NewHost() *Host { return &Host{} }

// EnsureCache is a no-op: swarm creates the mover's node-local cache volume on
// demand when the mover service mounts it.
func (Host) EnsureCache(context.Context, string) error { return nil }

// VolumeHostPath cannot resolve a node-local volume from the manager, so it
// reports the source as non-local — snapshotting then treats it as live.
func (Host) VolumeHostPath(context.Context, string) (hostPath, foreign string, err error) {
	return "", "swarm: node-local volumes are not resolvable from a manager", nil
}

// RunningMoverBinds returns nothing: swarm has no snapshots to reap and movers
// are services, not local containers.
func (Host) RunningMoverBinds(context.Context) ([]string, error) { return nil, nil }
