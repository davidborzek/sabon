// Package zfs implements the "zfs" snapshot strategy: it resolves each source's
// host path to the ZFS dataset that holds it, takes `zfs snapshot`s, and
// exposes them read-only via each dataset's `.zfs/snapshot/<name>/` directory,
// which the orchestrator bind-mounts into the mover instead of the live path.
//
// zfs is host-privileged, so the actual `zfs` calls run in an ephemeral
// privileged snapshotter container that enters the host namespaces with nsenter and
// invokes the host's own `zfs` binary (avoiding userland/kernel version skew).
// sabon's own process stays unprivileged.
package zfs

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/davidborzek/sabon/internal/snapshot"
	"github.com/docker/docker/client"
)

// runner runs a `zfs` subcommand on the host and returns its stdout. The
// concrete implementation (dockerRunner) spawns the privileged snapshotter; tests
// substitute a fake.
type runner interface {
	run(ctx context.Context, args ...string) (string, error)
	// reap removes leftover snapshotter containers from a previous crash.
	reap(ctx context.Context) (int, error)
}

type snapshotter struct {
	runner   runner
	instance string // SABON_INSTANCE; namespaces snapshot names so reap stays scoped
	log      *slog.Logger
}

// New returns a ZFS Snapshotter that runs `zfs` in a privileged snapshotter spawned
// from image. instance (SABON_INSTANCE) namespaces snapshot names so Reap only
// ever touches this instance's own snapshots.
func New(cli client.APIClient, image, instance string, log *slog.Logger) snapshot.Snapshotter {
	return &snapshotter{runner: newDockerRunner(cli, image, log), instance: instance, log: log}
}

// Reap removes leftover snapshot infrastructure from a previous crash: exited
// snapshotter containers and orphaned snapshots belonging to THIS instance
// (namespaced by SABON_INSTANCE). Call it at startup, before any backup runs —
// every such snapshot present then is an orphan, and scoping to the instance
// avoids destroying a sibling instance's live snapshots.
func (z *snapshotter) Reap(ctx context.Context, inUsePaths []string) (int, error) {
	n, err := z.runner.reap(ctx)
	if err != nil {
		return n, err
	}
	inUse := map[string]bool{}
	for _, p := range inUsePaths {
		if name := snapshotNameFromPath(p); name != "" {
			inUse[name] = true
		}
	}
	out, err := z.runner.run(ctx, "list", "-t", "snapshot", "-H", "-o", "name")
	if err != nil {
		return n, fmt.Errorf("zfs list snapshots: %w", err)
	}
	for _, full := range strings.Split(strings.TrimSpace(out), "\n") {
		if !z.ownsSnapshot(full) {
			continue
		}
		if _, snap, _ := strings.Cut(full, "@"); inUse[snap] {
			continue // a running mover is still reading it — spare it
		}
		if _, err := z.runner.run(ctx, "destroy", full); err != nil {
			z.log.Warn("failed to destroy orphan snapshot", "snapshot", full, "error", err)
			continue
		}
		n++
	}
	return n, nil
}

// snapPrefix is the snapshot-name prefix identifying this instance's snapshots
// ("sabon-<instance>-"), used for both naming and scoped reaping.
func (z *snapshotter) snapPrefix() string {
	inst := z.instance
	if inst == "" {
		inst = "default"
	}
	return "sabon-" + sanitize(inst) + "-"
}

// ownsSnapshot reports whether a `<dataset>@<snap>` name is one this instance took.
func (z *snapshotter) ownsSnapshot(name string) bool {
	_, snap, ok := strings.Cut(name, "@")
	return ok && strings.HasPrefix(snap, z.snapPrefix())
}

// snapshotNameFromPath extracts the snapshot name from a mover bind source like
// "<mountpoint>/.zfs/snapshot/<name>/<sub>"; empty when the path isn't one.
func snapshotNameFromPath(p string) string {
	const marker = "/.zfs/snapshot/"
	i := strings.Index(p, marker)
	if i < 0 {
		return ""
	}
	name, _, _ := strings.Cut(p[i+len(marker):], "/")
	return name
}

func (z *snapshotter) Mode() string { return "zfs" }

// Resolve lists the datasets once and maps each source's host path to the
// dataset that backs it (empty when not on ZFS). The `zfs list` doubles as the
// host-availability check: a non-nil error means zfs is unusable here.
func (z *snapshotter) Resolve(ctx context.Context, sources []snapshot.Source) ([]snapshot.Resolution, error) {
	out, err := z.runner.run(ctx, "list", "-H", "-o", "name,mountpoint", "-t", "filesystem")
	if err != nil {
		return nil, fmt.Errorf("zfs unavailable on host: %w", err)
	}
	datasets := parseDatasets(out)
	res := make([]snapshot.Resolution, 0, len(sources))
	for _, s := range sources {
		r := snapshot.Resolution{Name: s.Name, HostPath: s.HostPath, Detail: "not on a ZFS dataset"}
		if ds, _, ok := datasetFor(s.HostPath, datasets); ok {
			r.Snapshottable = true
			r.Detail = "dataset " + ds.name
		}
		res = append(res, r)
	}
	return res, nil
}

func (z *snapshotter) Snapshot(ctx context.Context, app string, sources []snapshot.Source) ([]snapshot.Mount, func(context.Context), error) {
	out, err := z.runner.run(ctx, "list", "-H", "-o", "name,mountpoint", "-t", "filesystem")
	if err != nil {
		return nil, nil, fmt.Errorf("zfs list: %w", err)
	}
	datasets := parseDatasets(out)

	// Resolve every source to its dataset up front, so a non-ZFS source fails
	// before any snapshot is taken (nothing to clean up).
	suffix := z.snapSuffix(app)
	snapByDS := map[string]string{} // dataset -> "<dataset>@<suffix>"
	var snapNames []string
	var mounts []snapshot.Mount
	for _, s := range sources {
		ds, sub, ok := datasetFor(s.HostPath, datasets)
		if !ok {
			return nil, nil, fmt.Errorf("snapshot=zfs: source %q (%s) is not on a ZFS dataset", s.Name, s.HostPath)
		}
		if _, seen := snapByDS[ds.name]; !seen {
			snapByDS[ds.name] = ds.name + "@" + suffix
			snapNames = append(snapNames, snapByDS[ds.name])
		}
		mounts = append(mounts, snapshot.Mount{Name: s.Name, HostPath: readPath(ds.mountpoint, suffix, sub)})
	}

	// One `zfs snapshot` for every dataset at once: atomic (a single TXG, so
	// sources on different datasets are mutually consistent) and one snapshotter
	// spawn instead of one per dataset. On failure ZFS creates none, so there
	// is nothing to clean up.
	if _, err := z.runner.run(ctx, append([]string{"snapshot"}, snapNames...)...); err != nil {
		return nil, nil, fmt.Errorf("zfs snapshot: %w", err)
	}
	z.log.Debug("created zfs snapshot", "app", app, "snapshots", snapNames)

	cleanup := func(ctx context.Context) {
		z.log.Debug("destroying zfs snapshot", "snapshots", snapNames)
		for _, snap := range snapNames {
			if _, err := z.runner.run(ctx, "destroy", snap); err != nil {
				z.log.Warn("failed to destroy zfs snapshot", "snapshot", snap, "error", err)
			}
		}
	}
	return mounts, cleanup, nil
}

type dataset struct {
	name, mountpoint string
}

// parseDatasets parses `zfs list -H -o name,mountpoint` output (tab-separated),
// skipping datasets without a real mountpoint.
func parseDatasets(out string) []dataset {
	var ds []dataset
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\t", 2)
		if len(f) != 2 {
			continue
		}
		mp := strings.TrimSpace(f[1])
		switch mp {
		case "", "-", "none", "legacy":
			continue
		}
		ds = append(ds, dataset{name: strings.TrimSpace(f[0]), mountpoint: mp})
	}
	return ds
}

// datasetFor finds the dataset whose mountpoint is the longest prefix of path
// and returns it plus the sub-path within it. ok is false when no dataset
// contains path (i.e. it is not on ZFS).
func datasetFor(path string, ds []dataset) (dataset, string, bool) {
	path = filepath.Clean(path)
	var best dataset
	var bestSub string
	bestLen := -1
	for _, d := range ds {
		mp := filepath.Clean(d.mountpoint)
		var sub string
		switch {
		case mp == path:
			sub = ""
		case mp == "/":
			sub = strings.TrimPrefix(path, "/")
		case strings.HasPrefix(path, mp+"/"):
			sub = strings.TrimPrefix(path, mp+"/")
		default:
			continue
		}
		if len(mp) > bestLen {
			best, bestSub, bestLen = d, sub, len(mp)
		}
	}
	if bestLen < 0 {
		return dataset{}, "", false
	}
	return best, bestSub, true
}

// readPath is where a dataset's snapshot exposes sub read-only.
func readPath(mountpoint, snap, sub string) string {
	base := filepath.Join(mountpoint, ".zfs", "snapshot", snap)
	if sub == "" {
		return base
	}
	return filepath.Join(base, sub)
}

// snapSuffix builds a unique, zfs-safe snapshot name for an app, prefixed with
// this instance's namespace.
func (z *snapshotter) snapSuffix(app string) string {
	return z.snapPrefix() + sanitize(app) + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
}

// sanitize keeps only characters valid in a zfs snapshot name.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == ':':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "app"
	}
	return b.String()
}
