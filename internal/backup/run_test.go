package backup

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/davidborzek/sabon/api"
	"github.com/davidborzek/sabon/internal/discovery"
	"github.com/davidborzek/sabon/internal/engine"
	"github.com/davidborzek/sabon/internal/mover"
	"github.com/davidborzek/sabon/internal/snapshot"
)

type runEngine struct {
	engine.Engine
	run func(context.Context, mover.Request) (mover.Result, bool, error)
}

func (f runEngine) RunMover(ctx context.Context, req mover.Request) (mover.Result, bool, error) {
	return f.run(ctx, req)
}

type runHooks struct {
	engine.Hooks
	run func(context.Context, engine.RunSpec) error
}

func (f runHooks) RunOneShot(ctx context.Context, spec engine.RunSpec) error {
	return f.run(ctx, spec)
}

type runQuiescer struct {
	stop, start func(context.Context, string) error
}

func (f runQuiescer) Stop(ctx context.Context, ref string) error  { return f.stop(ctx, ref) }
func (f runQuiescer) Start(ctx context.Context, ref string) error { return f.start(ctx, ref) }

type runSnapshot struct {
	fakeSnap
	err     error
	cleanup func(context.Context)
}

func (f runSnapshot) Snapshot(ctx context.Context, app string, sources []snapshot.Source) ([]snapshot.Mount, func(context.Context), error) {
	if f.err != nil {
		return nil, nil, f.err
	}
	ms, _, err := f.fakeSnap.Snapshot(ctx, app, sources)
	return ms, f.cleanup, err
}

func checkCleanupContext(t *testing.T, ctx context.Context) {
	t.Helper()
	deadline, ok := ctx.Deadline()
	if ctx.Err() != nil || !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Minute {
		t.Errorf("cleanup must have a fresh bounded context: err=%v deadline=%v", ctx.Err(), deadline)
	}
}

func TestRunTargetRecovery(t *testing.T) {
	primary, recovery := errors.New("primary"), errors.New("recovery")
	for _, tt := range []struct {
		name                                 string
		pre, stop, source, move, post, start error
		cancel, kept                         bool
		wantCalls                            []string
	}{
		{name: "post failure", post: recovery, wantCalls: []string{"pre", "stop", "move", "post", "snapshot", "start"}},
		{name: "restart failure", start: recovery, wantCalls: []string{"pre", "stop", "move", "post", "snapshot", "start"}},
		{name: "source failure", source: primary, wantCalls: []string{"pre", "stop", "post", "start"}},
		{name: "stop failure", stop: primary, wantCalls: []string{"pre", "stop", "post"}},
		{name: "primary and cleanup failures", move: primary, post: recovery, start: recovery, wantCalls: []string{"pre", "stop", "move", "post", "snapshot", "start"}},
		{name: "canceled backup", cancel: true, move: context.Canceled, post: recovery, wantCalls: []string{"pre", "stop", "move", "post", "snapshot", "start"}},
		{name: "failed prehook", pre: primary, wantCalls: []string{"pre"}},
		{name: "timed out prehook", pre: context.DeadlineExceeded, wantCalls: []string{"pre"}},
		{name: "retained mover", cancel: true, kept: true, move: context.Canceled, wantCalls: []string{"pre", "move"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("RESTIC_PASSWORD", "secret")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls []string
			var postCtx context.Context
			o := testOrch("zfs")
			o.locks, o.colds = newKeyedMutex(), newColdStops()
			o.cacheOK = true
			o.hooks = runHooks{run: func(ctx context.Context, spec engine.RunSpec) error {
				calls = append(calls, spec.Image)
				if spec.Image == "pre" {
					return tt.pre
				}
				checkCleanupContext(t, ctx)
				postCtx = ctx
				return tt.post
			}}
			o.quiesce = runQuiescer{
				stop: func(context.Context, string) error { calls = append(calls, "stop"); return tt.stop },
				start: func(ctx context.Context, _ string) error {
					checkCleanupContext(t, ctx)
					if postCtx == nil || postCtx.Err() != context.Canceled {
						t.Error("restart should have its own context after post cleanup ends")
					}
					calls = append(calls, "start")
					return tt.start
				},
			}
			o.snaps = []snapshot.Snapshotter{&runSnapshot{
				fakeSnap: fakeSnap{mode: "zfs", onFS: map[string]bool{"a": true}}, err: tt.source,
				cleanup: func(ctx context.Context) { checkCleanupContext(t, ctx); calls = append(calls, "snapshot") },
			}}
			o.eng = runEngine{run: func(_ context.Context, req mover.Request) (mover.Result, bool, error) {
				calls = append(calls, "move")
				if tt.cancel {
					cancel()
				}
				if tt.kept && !req.KeepOnShutdown {
					t.Fatal("retained mover must be a hot backup")
				}
				return mover.Result{SnapshotID: "created"}, tt.kept, tt.move
			}}
			job := discovery.Job{App: "app", Container: "container", Sources: binds("a"), Spec: api.Spec{
				Stop: !tt.kept, PreHooks: []api.Hook{{Image: "pre"}}, PostHooks: []api.Hook{{Image: "post"}},
			}}
			res, err := o.RunTarget(ctx, job, api.Target{Name: "local", Path: "/backup"})
			for _, want := range []error{tt.pre, tt.stop, tt.source, tt.move, tt.post, tt.start} {
				if want != nil && !errors.Is(err, want) {
					t.Errorf("error %v does not contain %v", err, want)
				}
			}
			if tt.pre == nil && tt.stop == nil && tt.source == nil && res.SnapshotID != "created" {
				t.Errorf("lost mover result on recovery failure: %+v", res)
			}
			if !reflect.DeepEqual(calls, tt.wantCalls) {
				t.Errorf("calls = %v, want %v", calls, tt.wantCalls)
			}
		})
	}
}

func TestRunRestoreRestartFailure(t *testing.T) {
	t.Setenv("RESTIC_PASSWORD", "secret")
	restartErr, moveErr := errors.New("restart"), errors.New("restore")
	for _, primary := range []error{nil, moveErr} {
		o := testOrch("none")
		o.locks, o.cacheOK = newKeyedMutex(), true
		ctx, cancel := context.WithCancel(context.Background())
		o.eng = runEngine{run: func(context.Context, mover.Request) (mover.Result, bool, error) {
			cancel()
			return mover.Result{}, false, primary
		}}
		o.quiesce = runQuiescer{
			stop: func(context.Context, string) error { return nil },
			start: func(ctx context.Context, _ string) error {
				checkCleanupContext(t, ctx)
				return restartErr
			},
		}
		err := o.RunRestore(ctx, "app", &discovery.Job{Container: "container"}, api.Target{Path: "/backup"}, RestoreOptions{Stop: true}, io.Discard)
		cancel()
		if !errors.Is(err, restartErr) || (primary != nil && !errors.Is(err, primary)) {
			t.Errorf("restore lost primary/restart failure: %v", err)
		}
	}
}

func TestRunTargetFinalColdHolderGetsRestartError(t *testing.T) {
	t.Setenv("RESTIC_PASSWORD", "secret")
	o := testOrch("none")
	o.locks, o.colds, o.cacheOK = newKeyedMutex(), newColdStops(), true
	restartErr := errors.New("restart")
	o.quiesce = runQuiescer{
		stop:  func(context.Context, string) error { return nil },
		start: func(context.Context, string) error { return restartErr },
	}
	entered, finish := make(chan struct{}), make(chan struct{})
	o.eng = runEngine{run: func(_ context.Context, req mover.Request) (mover.Result, bool, error) {
		if req.Labels[mover.LabelTarget] == "last" {
			close(entered)
			<-finish
		}
		return mover.Result{}, false, nil
	}}
	job := discovery.Job{App: "app", Container: "container", Spec: api.Spec{Stop: true}}
	done := make(chan error, 1)
	go func() {
		_, err := o.RunTarget(context.Background(), job, api.Target{Name: "last", Path: "/backup"})
		done <- err
	}()
	<-entered
	_, err := o.RunTarget(context.Background(), job, api.Target{Name: "first", Path: "/backup"})
	close(finish)
	if err != nil {
		t.Errorf("non-final holder got restart error: %v", err)
	}
	if err := <-done; !errors.Is(err, restartErr) {
		t.Errorf("final holder lost restart error: %v", err)
	}
}
