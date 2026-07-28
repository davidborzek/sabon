package docker

// Host implements engine.Host for the standalone Docker daemon: creating the
// cache volume, resolving a named volume to its host mountpoint, and finding
// host binds mounted into running movers (so a snapshot reap spares live ones).

import (
	"context"
	"fmt"

	"github.com/davidborzek/sabon/internal/engine"
	"github.com/davidborzek/sabon/internal/mover"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

var _ engine.Host = (*Host)(nil)

// Host provides node-local host/volume access against the local Docker daemon.
type Host struct {
	cli client.APIClient
}

// NewHost returns a Docker host accessor.
func NewHost(cli client.APIClient) *Host { return &Host{cli: cli} }

// EnsureCache creates the shared restic cache volume if it does not exist.
func (h *Host) EnsureCache(ctx context.Context, name string) error {
	if _, err := h.cli.VolumeCreate(ctx, volume.CreateOptions{Name: name}); err != nil {
		return err
	}
	return nil
}

// VolumeHostPath resolves a named volume to its host mountpoint, flagging a
// non-local (plugin/nfs/…) backing that cannot be filesystem-snapshotted.
func (h *Host) VolumeHostPath(ctx context.Context, name string) (hostPath, foreign string, err error) {
	v, err := h.cli.VolumeInspect(ctx, name)
	if err != nil {
		return "", "", fmt.Errorf("inspect volume %q: %w", name, err)
	}
	reason, _ := nonLocalBacking(v)
	return v.Mountpoint, reason, nil
}

// RunningMoverBinds returns the host bind sources mounted into running movers.
func (h *Host) RunningMoverBinds(ctx context.Context) ([]string, error) {
	list, err := h.cli.ContainerList(ctx, container.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", mover.LabelKey)),
	})
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, c := range list {
		insp, err := h.cli.ContainerInspect(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		for _, m := range insp.Mounts {
			paths = append(paths, m.Source)
		}
	}
	return paths, nil
}

// nonLocalBacking reports whether a named volume's data lives somewhere other
// than the host's local filesystem, with a reason for diagnostics. A non-local
// driver is plugin-backed; a local driver carrying mount options mounts a
// foreign filesystem (nfs/cifs/…) over its mountpoint. ok is false for a plain
// local volume — a real directory on the host filesystem.
func nonLocalBacking(v volume.Volume) (reason string, ok bool) {
	if v.Driver != "local" {
		return fmt.Sprintf("plugin-backed volume (driver %q), not local storage", v.Driver), true
	}
	if len(v.Options) > 0 {
		if t := v.Options["type"]; t != "" {
			return fmt.Sprintf("%s mount, not local storage", t), true
		}
		return "foreign mount (driver options set), not local storage", true
	}
	return "", false
}
