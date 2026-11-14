package docker

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

type quiesceDocker struct {
	client.APIClient
	id                            string
	running                       bool
	inspectErr, stopErr, startErr error
	inspects                      int
	stops, starts                 []string
}

func (f *quiesceDocker) ContainerInspect(context.Context, string) (container.InspectResponse, error) {
	f.inspects++
	return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{
		ID: f.id, State: &container.State{Running: f.running},
	}}, f.inspectErr
}

func (f *quiesceDocker) ContainerStop(_ context.Context, id string, _ container.StopOptions) error {
	f.stops = append(f.stops, id)
	f.running = false // even an error may follow a successful daemon-side stop
	return f.stopErr
}

func (f *quiesceDocker) ContainerStart(_ context.Context, id string, _ container.StartOptions) error {
	f.starts = append(f.starts, id)
	return f.startErr
}

func TestQuiesceOriginalState(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(map[bool]string{false: "stopped", true: "running"}[running], func(t *testing.T) {
			f := &quiesceDocker{id: "original", running: running}
			q := NewQuiescer(f)
			ctx := context.Background()
			// Repeated concurrent stops must preserve the first observation.
			var wg sync.WaitGroup
			for range 8 {
				wg.Go(func() {
					if err := q.Stop(ctx, "app"); err != nil {
						t.Error(err)
					}
				})
			}
			wg.Wait()
			f.id = "replacement" // the name now resolves to a different container
			if err := q.Start(ctx, "app"); err != nil {
				t.Fatal(err)
			}
			if err := q.Start(ctx, "app"); err != nil {
				t.Fatal(err)
			}
			want := 0
			if running {
				want = 1
			}
			if len(f.starts) != want || (running && f.starts[0] != "original") {
				t.Fatalf("starts = %v", f.starts)
			}
			if f.inspects != 1 {
				t.Fatalf("inspects = %d", f.inspects)
			}
			for _, id := range f.stops {
				if id != "original" {
					t.Fatalf("stopped %s", id)
				}
			}
			// A completed cycle must allow fresh state on the next operation.
			if err := q.Stop(ctx, "app"); err != nil {
				t.Fatal(err)
			}
			if err := q.Start(ctx, "app"); err != nil {
				t.Fatal(err)
			}
			if f.inspects != 2 || len(f.starts) != want {
				t.Fatal("state leaked into next cycle")
			}
		})
	}
}

func TestQuiesceErrors(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("daemon unavailable")
	f := &quiesceDocker{id: "original", running: true, inspectErr: boom}
	q := NewQuiescer(f)
	if err := q.Stop(ctx, "app"); !errors.Is(err, boom) {
		t.Fatalf("inspect error = %v", err)
	}
	if err := q.Start(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	if len(f.stops)+len(f.starts) != 0 {
		t.Fatal("acted after failed inspect")
	}
	f.inspectErr, f.stopErr = nil, boom
	if err := q.Stop(ctx, "app"); !errors.Is(err, boom) {
		t.Fatalf("stop error = %v", err)
	}
	f.stopErr = nil
	f.id = "replacement"
	if err := q.Stop(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	f.startErr = boom
	if err := q.Start(ctx, "app"); !errors.Is(err, boom) {
		t.Fatalf("start error = %v", err)
	}
	// A failed restart retains the original ID/state for subsequent retries.
	if err := q.Stop(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	f.startErr = nil
	if err := q.Start(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	if len(f.starts) != 2 || f.starts[0] != "original" || f.starts[1] != "original" {
		t.Fatalf("starts = %v", f.starts)
	}
	if f.inspects != 2 {
		t.Fatalf("re-inspected after error: %d", f.inspects)
	}
}
