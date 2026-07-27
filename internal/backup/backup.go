// Package backup orchestrates a single backup run for one app to one target:
// it acquires the per-repository lock, runs pre-hooks (optionally stopping the
// app), spawns the mover with the right mounts and environment, then runs
// post-hooks. It shells no restic itself — the mover does.
package backup

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/davidborzek/sabon/internal/config"
	"github.com/davidborzek/sabon/internal/hook"
	"github.com/davidborzek/sabon/internal/mover"
	"github.com/davidborzek/sabon/internal/snapshot"
	"github.com/davidborzek/sabon/internal/snapshot/providers/zfs"
	"github.com/docker/docker/client"
)

const (
	dataMount           = "/data"
	repoMount           = "/repo"
	cacheMount          = "/cache"
	restoreMount        = "/restore"
	passwordMount       = "/run/secrets/sabon-restic-password"
	awsCredentialsMount = "/run/secrets/sabon-aws-credentials"
)

// Orchestrator runs backups. It is safe for concurrent use across different
// repositories; runs against the same repository are serialised.
type Orchestrator struct {
	cli     client.APIClient
	cfg     *config.Config
	runner  *mover.Runner
	hooks   *hook.Runner
	image   string
	log     *slog.Logger
	locks   keyedMutex
	cacheMu sync.Mutex
	cacheOK bool
	sem     chan struct{}          // caps concurrent backups when SABON_MAX_PARALLEL > 0
	snaps   []snapshot.Snapshotter // registered source-snapshot providers; selected per run by mode
	colds   coldStops              // coordinates cold-backup container stops
}

// New returns an Orchestrator. image is the resolved mover image reference.
func New(cli client.APIClient, cfg *config.Config, image string, log *slog.Logger) *Orchestrator {
	o := &Orchestrator{
		cli:    cli,
		cfg:    cfg,
		runner: mover.NewRunner(cli),
		hooks:  hook.New(cli),
		image:  image,
		log:    log,
		snaps:  []snapshot.Snapshotter{zfs.New(cli, cfg.SnapshotZFSImage, cfg.Instance, log)},
		locks:  newKeyedMutex(),
		colds:  newColdStops(),
	}
	if cfg.MaxParallel > 0 {
		o.sem = make(chan struct{}, cfg.MaxParallel)
	}
	return o
}

// acquire blocks for a concurrency slot (no-op when unlimited).
func (o *Orchestrator) acquire(ctx context.Context) error {
	if o.sem == nil {
		return nil
	}
	select {
	case o.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (o *Orchestrator) release() {
	if o.sem != nil {
		<-o.sem
	}
}

// Reap removes exited leftover mover containers from a previous crash. Safe to
// call periodically: running movers are spared.
func (o *Orchestrator) Reap(ctx context.Context) (int, error) {
	n, err := o.runner.Reap(ctx)
	hn, herr := o.hooks.Reap(ctx)
	if err == nil {
		err = herr
	}
	return n + hn, err
}

// keyedMutex serialises operations sharing a key.
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func newKeyedMutex() keyedMutex { return keyedMutex{m: map[string]*sync.Mutex{}} }

func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	mu, ok := k.m[key]
	if !ok {
		mu = &sync.Mutex{}
		k.m[key] = mu
	}
	k.mu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// coldStops coordinates cold-backup container stops across concurrent runs of
// the same container (an app backed up to several targets at once): the first
// run to need it stops the container, the last to finish starts it again — so
// overlapping cold backups share one downtime and none reads a container that a
// sibling run has already restarted.
type coldStops struct {
	mu sync.Mutex
	m  map[string]*coldStop
}

func newColdStops() coldStops { return coldStops{m: map[string]*coldStop{}} }

func (c *coldStops) get(container string) *coldStop {
	c.mu.Lock()
	defer c.mu.Unlock()
	cs, ok := c.m[container]
	if !ok {
		cs = &coldStop{}
		c.m[container] = cs
	}
	return cs
}

// coldStop reference-counts one container's cold-backup holders.
type coldStop struct {
	mu   sync.Mutex
	refs int
}

// hold registers a cold-backup holder, running stop the first time (refs 0->1).
// A failed stop is not counted, so the next holder retries it.
func (c *coldStop) hold(stop func() error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refs == 0 {
		if err := stop(); err != nil {
			return err
		}
	}
	c.refs++
	return nil
}

// release drops a holder, running start when the last one leaves (refs 1->0).
func (c *coldStop) release(start func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refs--
	if c.refs == 0 {
		start()
	}
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, s)
}
