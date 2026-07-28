package swarm

import (
	"context"
	"testing"
	"time"

	"github.com/davidborzek/sabon/internal/engine"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"
)

func testHooks(f *fakeSwarm) *Hooks { return &Hooks{cli: f, poll: time.Millisecond} }

func TestSwarmHooksNoExecer(t *testing.T) {
	// Swarm does not provide exec-mode hooks, so it must not satisfy
	// engine.Execer — the orchestrator then falls back to a clear error.
	var h engine.Hooks = &Hooks{}
	if _, ok := h.(engine.Execer); ok {
		t.Error("swarm Hooks must not implement engine.Execer")
	}
}

func TestSwarmHooksRunOneShot(t *testing.T) {
	f := &fakeSwarm{tasks: []swarm.Task{completeTask(0)}}
	spec := engine.RunSpec{
		Name:    "sabon-hook-x",
		Image:   "busybox",
		Command: []string{"sh", "-c", "dump"},
		Volumes: []string{"dumps:/dump", "/srv/x:/x:ro"},
	}
	if err := testHooks(f).RunOneShot(context.Background(), spec); err != nil {
		t.Fatalf("RunOneShot: %v", err)
	}
	cs := f.created.TaskTemplate.ContainerSpec
	if cs == nil || cs.Image != "busybox" {
		t.Fatalf("container spec image wrong: %+v", cs)
	}
	if rp := f.created.TaskTemplate.RestartPolicy; rp == nil || rp.Condition != swarm.RestartPolicyConditionNone {
		t.Errorf("restart policy must be 'none': %+v", rp)
	}
	if f.created.Labels[engine.HookLabelKey] != "sabon-hook-x" {
		t.Errorf("hook label missing: %v", f.created.Labels)
	}
	if len(cs.Mounts) != 2 || cs.Mounts[0].Type != mount.TypeVolume || cs.Mounts[0].Source != "dumps" || !cs.Mounts[1].ReadOnly {
		t.Errorf("mounts wrong: %+v", cs.Mounts)
	}
	if len(f.removed) != 1 || f.removed[0] != "svc1" {
		t.Errorf("one-shot hook service must be removed after the run; removed=%v", f.removed)
	}
}

func TestSwarmHooksRunOneShotFailure(t *testing.T) {
	f := &fakeSwarm{tasks: []swarm.Task{{Status: swarm.TaskStatus{State: swarm.TaskStateFailed, ContainerStatus: &swarm.ContainerStatus{ExitCode: 3}}}}}
	if err := testHooks(f).RunOneShot(context.Background(), engine.RunSpec{Name: "h", Image: "img"}); err == nil {
		t.Error("a failed hook task must return an error")
	}
}

func TestSwarmHooksReap(t *testing.T) {
	f := &fakeSwarm{services: []swarm.Service{{ID: "h1"}}, tasks: nil} // no running task => finished
	n, err := testHooks(f).Reap(context.Background())
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if n != 1 || len(f.removed) != 1 || f.removed[0] != "h1" {
		t.Errorf("finished hook service must be reaped; n=%d removed=%v", n, f.removed)
	}
}

func TestSwarmHooksAppEnv(t *testing.T) {
	f := &fakeSwarm{inspectSvc: swarm.Service{Spec: swarm.ServiceSpec{TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Env: []string{"FOO=bar", "BAZ=1"}}}}}}
	env, err := testHooks(f).AppEnv(context.Background(), "svc")
	if err != nil {
		t.Fatalf("AppEnv: %v", err)
	}
	if env["FOO"] != "bar" || env["BAZ"] != "1" {
		t.Errorf("env parsed wrong: %v", env)
	}
}
