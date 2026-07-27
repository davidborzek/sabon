package backup

import (
	"context"
	"fmt"
	"strings"

	"github.com/davidborzek/sabon/internal/discovery"
	"github.com/davidborzek/sabon/internal/mover"
	"github.com/davidborzek/sabon/internal/snapshot"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
)

// ReapSnapshotter removes leftover snapshot infrastructure (snapshotter containers
// and orphaned ZFS snapshots) from a previous crash. Call it ONLY at startup,
// before any backup runs — it cannot tell a live snapshot from an orphan.
func (o *Orchestrator) ReapSnapshotter(ctx context.Context) (int, error) {
	// Startup-only orphan reap, and only when a snapshotting mode is configured
	// (auto or a provider), so a plain install (SABON_SNAPSHOT=none) never pulls
	// or spawns a privileged snapshotter. (Per-app snapshot with a `none` global
	// forgoes startup orphan-reaping — a niche tradeoff for that clean default.)
	if o.cfg.Snapshot == "" || o.cfg.Snapshot == "none" {
		return 0, nil
	}
	inUse, err := o.snapshotsInUse(ctx)
	if err != nil {
		// Can't tell which snapshots a surviving mover still reads — skip the
		// reap rather than risk destroying an in-use one; retried next start.
		return 0, err
	}
	total := 0
	for _, snap := range o.snaps {
		n, err := snap.Reap(ctx, inUse)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// snapshotsInUse returns the host bind sources mounted into RUNNING movers, so
// a snapshot reap spares a snapshot a live backup (e.g. one that survived a
// shutdown) is still reading. It fails closed: any error listing or inspecting
// movers is returned so the caller skips reaping.
func (o *Orchestrator) snapshotsInUse(ctx context.Context) ([]string, error) {
	list, err := o.cli.ContainerList(ctx, container.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", mover.LabelKey)),
	})
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, c := range list {
		insp, err := o.cli.ContainerInspect(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		for _, m := range insp.Mounts {
			paths = append(paths, m.Source)
		}
	}
	return paths, nil
}

// PreviewSnapshots resolves a job's sources to their backing filesystem for
// `sabon validate` (no snapshot is taken). A non-nil error means the snapshot
// strategy is unusable on this host.
func (o *Orchestrator) PreviewSnapshots(ctx context.Context, job discovery.Job) ([]snapshot.Resolution, error) {
	sources, foreign, err := o.resolveHostPaths(ctx, job)
	if err != nil {
		return nil, err
	}
	mode := job.Spec.SnapshotMode(o.cfg.Snapshot)
	var candidates []snapshot.Snapshotter
	if mode == "auto" {
		candidates = o.snaps
	} else if snap := o.snapshotterFor(mode); snap != nil {
		candidates = []snapshot.Snapshotter{snap}
	}
	res := make([]snapshot.Resolution, len(sources))
	at := make(map[string]int, len(sources))
	for i, s := range sources {
		res[i] = snapshot.Resolution{Name: s.Name, HostPath: s.HostPath}
		at[s.Name] = i
		if reason, ok := foreign[s.Name]; ok {
			res[i].Detail = reason // foreign-backed: never snapshottable
		}
	}
	for _, snap := range candidates {
		rs, err := snap.Resolve(ctx, sources)
		if err != nil {
			if mode != "auto" {
				return nil, err // strict: surface host unavailability
			}
			continue // auto: an unavailable provider just means those sources go live
		}
		for _, r := range rs {
			i := at[r.Name]
			if _, isForeign := foreign[r.Name]; isForeign || res[i].Snapshottable {
				continue
			}
			if r.Snapshottable {
				res[i].Snapshottable = true
				res[i].Detail = r.Detail
			} else if res[i].Detail == "" {
				res[i].Detail = r.Detail
			}
		}
	}
	return res, nil
}

// sourceMountsFor builds the mover's source mounts for a job, honouring the
// snapshot strategy: "none" mounts every source live; a provider name (e.g.
// "zfs") snapshots every source with that provider, failing if one is not on its
// filesystem; "auto" snapshots each source with the first provider that can and
// mounts the rest live. The returned cleanup releases any snapshots taken.
func (o *Orchestrator) sourceMountsFor(ctx context.Context, job discovery.Job) ([]mount.Mount, func(context.Context), error) {
	noop := func(context.Context) {}
	o.log.Debug("resolving source mounts", "app", job.App, "mode", job.Spec.SnapshotMode(o.cfg.Snapshot), "sources", len(job.Sources))
	switch mode := job.Spec.SnapshotMode(o.cfg.Snapshot); mode {
	case "", "none":
		return o.sourceMounts(job, true), noop, nil
	case "auto":
		return o.autoMounts(ctx, job)
	default:
		snap := o.snapshotterFor(mode)
		if snap == nil {
			return nil, noop, fmt.Errorf("unknown snapshot mode %q (want none, auto, or a provider: %s)", mode, o.providerModes())
		}
		return o.strictMounts(ctx, job, snap)
	}
}

// strictMounts snapshots every source with one provider; a source not on that
// provider's filesystem (or a foreign-backed volume) fails the whole backup.
func (o *Orchestrator) strictMounts(ctx context.Context, job discovery.Job, snap snapshot.Snapshotter) ([]mount.Mount, func(context.Context), error) {
	noop := func(context.Context) {}
	sources, foreign, err := o.resolveHostPaths(ctx, job)
	if err != nil {
		return nil, noop, err
	}
	for _, s := range sources {
		if reason, ok := foreign[s.Name]; ok {
			return nil, noop, fmt.Errorf("snapshot=%s: source %q: %s — cannot snapshot; use a preHook dump or stop:true", snap.Mode(), s.Name, reason)
		}
	}
	snaps, cleanup, err := snap.Snapshot(ctx, job.App, sources)
	if err != nil {
		return nil, noop, err
	}
	ms := make([]mount.Mount, 0, len(snaps))
	for _, m := range snaps {
		ms = append(ms, mount.Mount{Type: mount.TypeBind, Source: m.HostPath, Target: dataMount + "/" + m.Name, ReadOnly: true})
	}
	return ms, cleanup, nil
}

// autoMounts snapshots each source with the first provider that reports it
// snapshottable (skipping foreign-backed volumes, whose mountpoint overlays a
// foreign filesystem) and mounts everything else live. It never fails a job for
// an un-snapshottable source — that is the point of auto versus a strict mode.
func (o *Orchestrator) autoMounts(ctx context.Context, job discovery.Job) ([]mount.Mount, func(context.Context), error) {
	noop := func(context.Context) {}
	sources, foreign, err := o.resolveHostPaths(ctx, job)
	if err != nil {
		return nil, noop, err
	}
	pending := make(map[string]bool, len(sources)) // eligible: not foreign-backed
	for _, s := range sources {
		if _, isForeign := foreign[s.Name]; !isForeign {
			pending[s.Name] = true
		}
	}
	// Each provider, in order, claims the pending sources it can snapshot.
	claimed := make(map[string]bool)
	perProvider := make([][]snapshot.Source, len(o.snaps))
	for i, snap := range o.snaps {
		if len(pending) == 0 {
			break
		}
		res, err := snap.Resolve(ctx, sources)
		if err != nil {
			o.log.Warn("snapshot=auto: provider unavailable, mounting its sources live", "provider", snap.Mode(), "error", err)
			continue
		}
		for _, r := range res {
			if pending[r.Name] && r.Snapshottable {
				perProvider[i] = append(perProvider[i], snapshot.Source{Name: r.Name, HostPath: r.HostPath})
				claimed[r.Name] = true
				delete(pending, r.Name)
			}
		}
	}
	// Snapshot each provider's claimed sources; roll back on a later failure.
	var cleanups []func(context.Context)
	ms := make([]mount.Mount, 0, len(job.Sources))
	for i, snap := range o.snaps {
		if len(perProvider[i]) == 0 {
			continue
		}
		snaps, cl, err := snap.Snapshot(ctx, job.App, perProvider[i])
		if err != nil {
			for _, c := range cleanups {
				c(context.WithoutCancel(ctx))
			}
			return nil, noop, err
		}
		cleanups = append(cleanups, cl)
		for _, m := range snaps {
			ms = append(ms, mount.Mount{Type: mount.TypeBind, Source: m.HostPath, Target: dataMount + "/" + m.Name, ReadOnly: true})
		}
	}
	// Everything not claimed is mounted live by its original type/ref, so a
	// volume mounts by name (driver-agnostic), not by its resolved path.
	for _, s := range job.Sources {
		if claimed[s.Name] {
			continue
		}
		ms = append(ms, mount.Mount{Type: s.Type, Source: s.Ref, Target: dataMount + "/" + s.Name, ReadOnly: true})
	}
	cleanup := func(ctx context.Context) {
		for _, c := range cleanups {
			c(ctx)
		}
	}
	return ms, cleanup, nil
}

// snapshotterFor returns the registered provider whose Mode matches, or nil.
func (o *Orchestrator) snapshotterFor(mode string) snapshot.Snapshotter {
	for _, s := range o.snaps {
		if s.Mode() == mode {
			return s
		}
	}
	return nil
}

// providerModes lists the registered provider mode names, for error messages.
func (o *Orchestrator) providerModes() string {
	modes := make([]string, len(o.snaps))
	for i, s := range o.snaps {
		modes[i] = s.Mode()
	}
	return strings.Join(modes, ", ")
}

// resolveHostPaths maps a job's sources to host paths: a bind's path as-is, a
// named volume's Mountpoint via inspect (so the ZFS dataset can be found). It
// also flags volumes whose data does not live on the host's local filesystem —
// a filesystem snapshot of such a mountpoint would capture the empty stub under
// it, not the overlaid data. The returned map is keyed by source name with a
// human-readable reason; only genuinely local sources are absent from it.
func (o *Orchestrator) resolveHostPaths(ctx context.Context, job discovery.Job) ([]snapshot.Source, map[string]string, error) {
	srcs := make([]snapshot.Source, 0, len(job.Sources))
	var foreign map[string]string
	for _, s := range job.Sources {
		hp := s.Ref
		if s.Type == mount.TypeVolume {
			v, err := o.cli.VolumeInspect(ctx, s.Ref)
			if err != nil {
				return nil, nil, fmt.Errorf("inspect volume %q: %w", s.Ref, err)
			}
			hp = v.Mountpoint
			if reason, ok := nonLocalBacking(v); ok {
				if foreign == nil {
					foreign = map[string]string{}
				}
				foreign[s.Name] = reason
			}
		}
		srcs = append(srcs, snapshot.Source{Name: s.Name, HostPath: hp})
	}
	return srcs, foreign, nil
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
