// Package snapshot defines the generic contract for source-snapshot strategies.
// A Snapshotter takes read-only, point-in-time copies of a job's sources so
// restic reads a crash-consistent view even while the app keeps writing.
//
// Concrete strategies live under providers/ (e.g. providers/zfs). The default
// is no snapshotter at all, in which case sources are mounted live.
package snapshot

import "context"

// Source is one thing to back up, identified by its /data/<Name> mount segment
// and its resolved host path (a bind path or a named volume's Mountpoint).
type Source struct {
	Name     string
	HostPath string
}

// Mount is a resolved read-only host path to bind into the mover at
// /data/<Name>.
type Mount struct {
	Name     string
	HostPath string
}

// Resolution is the outcome of resolving one source for diagnostics
// (sabon validate): whether the strategy can snapshot it, plus a
// provider-specific, human-readable detail (e.g. "dataset tank/appdata").
type Resolution struct {
	Name          string
	HostPath      string
	Snapshottable bool
	Detail        string
}

// Snapshotter prepares read-only snapshots of a job's sources.
type Snapshotter interface {
	// Snapshot prepares snapshots covering sources and returns the host paths to
	// read them from plus a cleanup that releases them. On error it leaks no
	// snapshot and the backup must not proceed.
	Snapshot(ctx context.Context, app string, sources []Source) ([]Mount, func(context.Context), error)
	// Mode is the strategy name (e.g. "zfs").
	Mode() string
	// Resolve reports, per source, the backing filesystem, for `sabon validate`.
	// A non-nil error means the strategy is not usable on this host.
	Resolve(ctx context.Context, sources []Source) ([]Resolution, error)
	// Reap removes leftover infrastructure (snapshotter containers and orphaned
	// snapshots) from a crash. inUsePaths are host paths still bind-mounted into
	// running movers; snapshots they reference are spared. Call at startup.
	Reap(ctx context.Context, inUsePaths []string) (int, error)
}
