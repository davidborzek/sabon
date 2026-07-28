// Package engine defines the runtime-agnostic contracts the backup orchestrator
// and reconciler depend on: running "mover" containers and reading their run
// history (Engine), discovering backup jobs (Discoverer), quiescing apps for
// cold backups (Quiescer), pre/post hooks (Hooks) and local host/volume access
// (Host). The concrete implementations live in the runtime
// subpackages: engine/docker (plain containers on the local daemon) and
// engine/swarm (one-shot services pinned across a cluster).
package engine

import (
	"context"
	"io"
	"time"

	"github.com/davidborzek/sabon/internal/discovery"
	"github.com/davidborzek/sabon/internal/mover"
)

// RunInfo is a past or in-flight mover run, reconstructed from its retained
// container/task so run history survives a sabon restart. The id is the mover
// container id, and its logs are the restic output.
type RunInfo struct {
	ID         string    `json:"id"`
	App        string    `json:"app"`
	Target     string    `json:"target"`
	Action     string    `json:"action"` // backup | check | prune | restore
	Status     string    `json:"status"` // running | succeeded | failed
	ExitCode   int       `json:"exit_code"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

// Engine runs movers and exposes their history. Implementations decide whether a
// mover is a plain container (standalone) or a one-shot service (Swarm).
type Engine interface {
	// RunMover creates, runs and waits for one mover, returning its result. The
	// bool reports whether the mover was intentionally left running (shutdown).
	RunMover(ctx context.Context, req mover.Request) (mover.Result, bool, error)
	// Reap trims exited movers, keeping the newest `keep` per app/target/action
	// as run history. Running movers are spared.
	Reap(ctx context.Context, keep int) (int, error)
	// ListRuns returns retained runs, newest first, optionally filtered.
	ListRuns(ctx context.Context, app, target string) ([]RunInfo, error)
	// GetRun returns one run by id; ok is false when none exists.
	GetRun(ctx context.Context, id string) (RunInfo, bool, error)
	// RunLogs writes a run's restic output to out.
	RunLogs(ctx context.Context, id string, out io.Writer) error
}

// Discoverer finds the backup jobs to manage and watches for changes.
// The standalone implementation lists sabon-labelled containers on the local
// daemon; a swarm implementation lists services across the cluster.
type Discoverer interface {
	List(ctx context.Context) ([]discovery.Job, error)
	Watch(ctx context.Context, onRestart func()) <-chan struct{}
}

// Quiescer stops an app for a cold (offline) backup or in-place restore and
// starts it again afterwards. Standalone stops the container; a swarm
// implementation scales the service to zero and back.
type Quiescer interface {
	Stop(ctx context.Context, ref string) error
	Start(ctx context.Context, ref string) error
}

// Recoverer is an optional Quiescer capability: it brings back apps a previous
// run stranded — e.g. a swarm service scaled to zero when the daemon crashed
// between Stop and Start. Runtimes with nothing to recover (standalone) omit it;
// the daemon discovers it by type assertion at startup.
type Recoverer interface {
	RecoverQuiesced(ctx context.Context) (int, error)
}

// Host is the local host/volume access the backup orchestrator needs for cache
// setup and snapshot source resolution — node-local operations only the
// standalone Docker daemon can do against its own state. The swarm runtime
// provides a no-op (movers are node-pinned services; snapshots are unsupported).
type Host interface {
	// EnsureCache makes sure the shared restic cache volume exists.
	EnsureCache(ctx context.Context, name string) error
	// VolumeHostPath resolves a named volume to its host mountpoint. foreign is a
	// non-empty reason when the volume's data is not on the local filesystem
	// (plugin/nfs/…) and so cannot be filesystem-snapshotted.
	VolumeHostPath(ctx context.Context, name string) (hostPath, foreign string, err error)
	// RunningMoverBinds returns host bind paths mounted into running movers, so a
	// snapshot reap spares one a live backup is still reading.
	RunningMoverBinds(ctx context.Context) ([]string, error)
}

// RunSpec configures a one-shot container/service hook (run mode).
type RunSpec struct {
	Name    string
	Image   string
	Command []string
	User    string
	Env     []string
	Network string
	Volumes []string // "source:/target[:ro]"; source = volume name or host path
}

// HookLabelKey marks one-shot hook containers/services so crashed or aborted
// ones can be reaped.
const HookLabelKey = "sabon.hook"

// Hooks runs pre/post backup actions against the runtime: a fresh one-shot
// container/service (run mode), reading the app's env and compose project, and
// reaping leftover one-shot hooks. Exec-mode hooks are a separate optional
// capability (Execer) — not every runtime can exec into a running app.
type Hooks interface {
	RunOneShot(ctx context.Context, spec RunSpec) error
	AppEnv(ctx context.Context, ref string) (map[string]string, error)
	AppProject(ctx context.Context, ref string) (string, error)
	Reap(ctx context.Context) (int, error)
}

// Execer is the optional exec-mode hook capability: run a command inside a live
// app instance. Standalone Docker provides it; a runtime that cannot exec into a
// running app across nodes (swarm) omits it, and callers fall back to an error.
type Execer interface {
	Exec(ctx context.Context, ref string, cmd, env []string, user string) error
}
