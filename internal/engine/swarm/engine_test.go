package swarm

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/davidborzek/sabon/internal/mover"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/swarm"
)

// fakeSwarm is the shared in-memory swarmClient fake for the swarm engine,
// quiescer, hooks and discoverer tests.
type fakeSwarm struct {
	created        swarm.ServiceSpec
	tasks          []swarm.Task
	services       []swarm.Service
	removed        []string
	inspectSvc     swarm.Service
	updated        []uint64
	nodes          []swarm.Node
	updatedLabels  []map[string]string // labels on each ServiceUpdate spec
	lastListLabels []string            // label filters from the last ServiceList
}

func (f *fakeSwarm) ServiceCreate(_ context.Context, spec swarm.ServiceSpec, _ swarm.ServiceCreateOptions) (swarm.ServiceCreateResponse, error) {
	f.created = spec
	return swarm.ServiceCreateResponse{ID: "svc1"}, nil
}
func (f *fakeSwarm) ServiceList(_ context.Context, opts swarm.ServiceListOptions) ([]swarm.Service, error) {
	f.lastListLabels = opts.Filters.Get("label")
	return f.services, nil
}
func (f *fakeSwarm) ServiceInspectWithRaw(context.Context, string, swarm.ServiceInspectOptions) (swarm.Service, []byte, error) {
	return f.inspectSvc, nil, nil
}
func (f *fakeSwarm) ServiceUpdate(_ context.Context, _ string, _ swarm.Version, spec swarm.ServiceSpec, _ swarm.ServiceUpdateOptions) (swarm.ServiceUpdateResponse, error) {
	if spec.Mode.Replicated != nil && spec.Mode.Replicated.Replicas != nil {
		f.updated = append(f.updated, *spec.Mode.Replicated.Replicas)
	}
	f.updatedLabels = append(f.updatedLabels, spec.Labels)
	f.inspectSvc.Spec = spec // persist so a later inspect sees the label change
	return swarm.ServiceUpdateResponse{}, nil
}
func (f *fakeSwarm) ServiceRemove(_ context.Context, id string) error {
	f.removed = append(f.removed, id)
	return nil
}
func (f *fakeSwarm) ServiceLogs(context.Context, string, container.LogsOptions) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (f *fakeSwarm) TaskList(context.Context, swarm.TaskListOptions) ([]swarm.Task, error) {
	return f.tasks, nil
}
func (f *fakeSwarm) NodeList(context.Context, swarm.NodeListOptions) ([]swarm.Node, error) {
	return f.nodes, nil
}
func (f *fakeSwarm) Events(context.Context, events.ListOptions) (<-chan events.Message, <-chan error) {
	return nil, nil
}

func completeTask(code int) swarm.Task {
	return swarm.Task{Status: swarm.TaskStatus{State: swarm.TaskStateComplete, ContainerStatus: &swarm.ContainerStatus{ExitCode: code}}}
}

func testSwarm(f *fakeSwarm) *Engine { return &Engine{cli: f, poll: time.Millisecond} }

func TestSwarmRunMoverBuildsService(t *testing.T) {
	f := &fakeSwarm{tasks: []swarm.Task{completeTask(0)}}
	repo := t.TempDir()
	req := mover.Request{
		Name:   "sabon-mover-demo-onsite-1",
		Image:  "ghcr.io/x/sabon:main",
		User:   "0:0",
		Node:   "node1",
		Spec:   mover.Spec{App: "demo"},
		Binds:  []string{repo + ":/repo"},
		Labels: map[string]string{mover.LabelAction: "backup", mover.LabelApp: "demo", mover.LabelTarget: "onsite"},
		Retain: true,
	}
	if _, _, err := testSwarm(f).RunMover(context.Background(), req); err != nil {
		t.Fatalf("RunMover: %v", err)
	}
	spec := f.created
	cs := spec.TaskTemplate.ContainerSpec
	if cs.Image != req.Image {
		t.Errorf("image = %q", cs.Image)
	}
	if len(cs.Command) != 2 || cs.Command[0] != "/sabon" || cs.Command[1] != "mover" {
		t.Errorf("command = %v, want [/sabon mover]", cs.Command)
	}
	if spec.TaskTemplate.RestartPolicy == nil || spec.TaskTemplate.RestartPolicy.Condition != swarm.RestartPolicyConditionNone {
		t.Errorf("restart policy not 'none': %+v", spec.TaskTemplate.RestartPolicy)
	}
	if spec.TaskTemplate.Placement == nil || len(spec.TaskTemplate.Placement.Constraints) != 1 || spec.TaskTemplate.Placement.Constraints[0] != "node.hostname==node1" {
		t.Errorf("placement = %+v", spec.TaskTemplate.Placement)
	}
	if spec.Mode.ReplicatedJob == nil || spec.Mode.ReplicatedJob.TotalCompletions == nil || *spec.Mode.ReplicatedJob.TotalCompletions != 1 {
		t.Errorf("mover mode not ReplicatedJob{1}: %+v", spec.Mode)
	}
	if cs.Labels[mover.LabelKey] != "demo" || cs.Labels[mover.LabelAction] != "backup" {
		t.Errorf("labels = %v", cs.Labels)
	}
	if len(cs.Mounts) != 1 || cs.Mounts[0].Source != repo || cs.Mounts[0].Target != "/repo" {
		t.Errorf("mounts = %+v", cs.Mounts)
	}
	if len(f.removed) != 0 {
		t.Errorf("retained mover must not be removed; removed=%v", f.removed)
	}
}

func TestSwarmRunMoverEphemeralRemoved(t *testing.T) {
	f := &fakeSwarm{tasks: []swarm.Task{completeTask(0)}}
	req := mover.Request{Name: "m", Image: "img", Spec: mover.Spec{App: "demo"}} // Retain=false
	if _, _, err := testSwarm(f).RunMover(context.Background(), req); err != nil {
		t.Fatalf("RunMover: %v", err)
	}
	if len(f.removed) != 1 || f.removed[0] != "svc1" {
		t.Errorf("ephemeral mover must be removed; removed=%v", f.removed)
	}
	if spec := f.created.TaskTemplate.Placement; spec != nil {
		t.Errorf("no Node => no placement constraint, got %+v", spec)
	}
}

func TestSwarmRunMoverFailed(t *testing.T) {
	f := &fakeSwarm{tasks: []swarm.Task{{Status: swarm.TaskStatus{State: swarm.TaskStateFailed, ContainerStatus: &swarm.ContainerStatus{ExitCode: 2}}}}}
	req := mover.Request{Name: "m", Image: "img", Spec: mover.Spec{App: "demo"}}
	if _, _, err := testSwarm(f).RunMover(context.Background(), req); err == nil {
		t.Error("a failed task must return an error")
	}
}

func TestSwarmQuiesce(t *testing.T) {
	three := uint64(3)
	f := &fakeSwarm{inspectSvc: swarm.Service{Spec: swarm.ServiceSpec{Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &three}}}}}
	q := &Quiescer{cli: f, drain: 0}
	if err := q.Stop(context.Background(), "app-svc"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := q.Start(context.Background(), "app-svc"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(f.updated) != 2 || f.updated[0] != 0 || f.updated[1] != 3 {
		t.Errorf("scale sequence = %v, want [0 3]", f.updated)
	}
}

func TestSwarmQuiesceRecover(t *testing.T) {
	zero := uint64(0)
	f := &fakeSwarm{services: []swarm.Service{{
		ID: "s1",
		Spec: swarm.ServiceSpec{
			Annotations: swarm.Annotations{Labels: map[string]string{"sabon.quiesce.replicas": "2"}},
			Mode:        swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &zero}},
		},
	}}}
	q := &Quiescer{cli: f}
	n, err := q.RecoverQuiesced(context.Background())
	if err != nil {
		t.Fatalf("RecoverQuiesced: %v", err)
	}
	if n != 1 || len(f.updated) != 1 || f.updated[0] != 2 {
		t.Errorf("stranded service must be restored to 2; n=%d updated=%v", n, f.updated)
	}
}

func TestSwarmQuiesceRecoverParkedStaysZero(t *testing.T) {
	zero := uint64(0)
	f := &fakeSwarm{services: []swarm.Service{{
		ID: "s1",
		Spec: swarm.ServiceSpec{
			Annotations: swarm.Annotations{Labels: map[string]string{"sabon.quiesce.replicas": "0"}},
			Mode:        swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &zero}},
		},
	}}}
	q := &Quiescer{cli: f}
	if _, err := q.RecoverQuiesced(context.Background()); err != nil {
		t.Fatalf("RecoverQuiesced: %v", err)
	}
	if len(f.updated) != 1 || f.updated[0] != 0 {
		t.Errorf("a legitimately parked (0) service must be restored to 0, not started; updated=%v", f.updated)
	}
}

func TestSwarmQuiesceInstanceScoped(t *testing.T) {
	one := uint64(1)
	f := &fakeSwarm{inspectSvc: swarm.Service{Spec: swarm.ServiceSpec{Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &one}}}}}
	q := &Quiescer{cli: f, instance: "prod", drain: 0}
	if err := q.Stop(context.Background(), "app-svc"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(f.updatedLabels) == 0 || f.updatedLabels[0]["sabon.quiesce.instance"] != "prod" {
		t.Errorf("Stop must tag the service with the owning instance; labels=%v", f.updatedLabels)
	}
	if _, err := q.RecoverQuiesced(context.Background()); err != nil {
		t.Fatalf("RecoverQuiesced: %v", err)
	}
	found := false
	for _, l := range f.lastListLabels {
		if l == "sabon.quiesce.instance=prod" {
			found = true
		}
	}
	if !found {
		t.Errorf("RecoverQuiesced must scope its query by the instance label; got %v", f.lastListLabels)
	}

	f2 := &fakeSwarm{}
	q2 := &Quiescer{cli: f2}
	if _, err := q2.RecoverQuiesced(context.Background()); err != nil {
		t.Fatalf("RecoverQuiesced (default): %v", err)
	}
	for _, l := range f2.lastListLabels {
		if strings.HasPrefix(l, "sabon.quiesce.instance") {
			t.Errorf("default instance must not scope by instance label; got %v", f2.lastListLabels)
		}
	}
}

func TestSwarmRunMoverTimeoutRemoved(t *testing.T) {
	f := &fakeSwarm{} // task never reaches a terminal state
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Minute))
	defer cancel()
	req := mover.Request{Name: "m", Image: "img", Spec: mover.Spec{App: "demo"}, Retain: true}
	if _, _, err := testSwarm(f).RunMover(ctx, req); err == nil {
		t.Error("a timed-out RunMover must return an error")
	}
	// Retain must NOT keep a still-running (timed-out) mover: it holds the repo
	// lock and must be removed.
	if len(f.removed) != 1 || f.removed[0] != "svc1" {
		t.Errorf("timed-out mover must be removed despite Retain; removed=%v", f.removed)
	}
}

func histSvc(id, app, target, action string) swarm.Service {
	return swarm.Service{ID: id, Spec: swarm.ServiceSpec{Annotations: swarm.Annotations{Labels: map[string]string{
		mover.LabelKey: app, mover.LabelApp: app, mover.LabelTarget: target, mover.LabelAction: action,
	}}}}
}

func TestSwarmReapStrayAndHistory(t *testing.T) {
	f := &fakeSwarm{services: []swarm.Service{
		{ID: "stray", Spec: swarm.ServiceSpec{Annotations: swarm.Annotations{Labels: map[string]string{mover.LabelKey: "demo"}}}}, // no action label
		histSvc("h1", "demo", "onsite", "backup"),
		histSvc("h2", "demo", "onsite", "backup"),
	}} // tasks nil => every service is terminated (serviceRunning == false)
	n, err := (&Engine{cli: f, poll: time.Millisecond}).Reap(context.Background(), 1)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if n != 2 {
		t.Errorf("reaped %d, want 2 (stray + one trimmed history mover)", n)
	}
	removed := map[string]bool{}
	for _, id := range f.removed {
		removed[id] = true
	}
	if !removed["stray"] {
		t.Errorf("action-less stray must be reaped; removed=%v", f.removed)
	}
	if removed["h1"] == removed["h2"] {
		t.Errorf("exactly one history mover must be trimmed to keep=1; removed=%v", f.removed)
	}
}

func TestSwarmRunMoverStreamedReturns(t *testing.T) {
	f := &fakeSwarm{tasks: []swarm.Task{completeTask(0)}}
	var out bytes.Buffer
	// Stdout set (snapshots/check/prune/restore): the streamed path must return,
	// not hang on a never-EOF `docker service logs --follow`.
	req := mover.Request{Name: "m", Image: "img", Spec: mover.Spec{App: "demo"}, Stdout: &out}
	if _, _, err := testSwarm(f).RunMover(context.Background(), req); err != nil {
		t.Fatalf("streamed RunMover: %v", err)
	}
	if len(f.removed) != 1 || f.removed[0] != "svc1" {
		t.Errorf("ephemeral streamed mover must be removed; removed=%v", f.removed)
	}
}
