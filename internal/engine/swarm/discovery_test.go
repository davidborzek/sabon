package swarm

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func svcWith(name, backup string, mounts []mount.Mount) swarm.Service {
	return swarm.Service{
		ID: "sid-" + name,
		Spec: swarm.ServiceSpec{
			Annotations: swarm.Annotations{
				Name:   name,
				Labels: map[string]string{"sabon.enable": "true", "sabon.backup": backup},
			},
			TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Mounts: mounts}},
		},
	}
}

func TestSwarmDiscoverList(t *testing.T) {
	f := &fakeSwarm{
		services: []swarm.Service{svcWith("demo-svc", "repo: demo\nexcludeVolumes: [scratch]", []mount.Mount{
			{Type: mount.TypeVolume, Source: "demo-data", Target: "/data"},
			{Type: mount.TypeVolume, Source: "scratch", Target: "/tmp"},   // excluded
			{Type: mount.TypeVolume, Source: "sabon-cache", Target: "/c"}, // cache volume
			{Type: mount.TypeBind, Source: "/srv/conf", Target: "/etc/app"},
		})},
		tasks: []swarm.Task{{NodeID: "nid1", DesiredState: swarm.TaskStateRunning}},
		nodes: []swarm.Node{{ID: "nid1", Description: swarm.NodeDescription{Hostname: "worker-1"}}},
	}
	d := NewDiscoverer(f, "sabon", false, "sabon-cache", "", discardLog())

	jobs, err := d.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	j := jobs[0]
	if j.App != "demo" || j.Container != "demo-svc" {
		t.Errorf("app=%q container=%q", j.App, j.Container)
	}
	if j.Node != "worker-1" {
		t.Errorf("node = %q, want worker-1", j.Node)
	}
	// demo-data volume + /srv/conf bind; scratch (excluded) and sabon-cache dropped.
	if len(j.Sources) != 2 {
		t.Fatalf("sources = %+v, want 2 (demo-data + bind)", j.Sources)
	}
	if j.Sources[0].Type != mount.TypeVolume || j.Sources[0].Ref != "demo-data" {
		t.Errorf("source[0] = %+v", j.Sources[0])
	}
	if j.Sources[1].Type != mount.TypeBind || j.Sources[1].Ref != "/srv/conf" {
		t.Errorf("source[1] = %+v", j.Sources[1])
	}
}

func TestSwarmDiscoverNoRunningTaskLeavesNodeEmpty(t *testing.T) {
	f := &fakeSwarm{
		services: []swarm.Service{svcWith("cold-svc", "repo: cold", []mount.Mount{{Type: mount.TypeVolume, Source: "cold-data", Target: "/d"}})},
		tasks:    nil, // scaled to zero: no running task
		nodes:    []swarm.Node{{ID: "nid1", Description: swarm.NodeDescription{Hostname: "worker-1"}}},
	}
	d := NewDiscoverer(f, "sabon", false, "sabon-cache", "", discardLog())
	jobs, err := d.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Node != "" {
		t.Fatalf("cold service must yield an empty Node; jobs=%+v", jobs)
	}
}

func TestSwarmDiscoverInstanceFilter(t *testing.T) {
	svc := svcWith("owned", "repo: owned", []mount.Mount{{Type: mount.TypeVolume, Source: "v", Target: "/d"}})
	svc.Spec.Labels["sabon.instance"] = "other"
	f := &fakeSwarm{services: []swarm.Service{svc}}
	d := NewDiscoverer(f, "sabon", false, "sabon-cache", "self", discardLog())
	jobs, err := d.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(jobs) != 0 {
		t.Errorf("service owned by another instance must be skipped; jobs=%+v", jobs)
	}
}

func TestSwarmDiscoverScaledToZeroUsesHostnameConstraint(t *testing.T) {
	svc := svcWith("cold-svc", "repo: cold", []mount.Mount{{Type: mount.TypeVolume, Source: "cold-data", Target: "/d"}})
	svc.Spec.TaskTemplate.Placement = &swarm.Placement{Constraints: []string{"node.hostname == worker-2"}}
	f := &fakeSwarm{
		services: []swarm.Service{svc},
		tasks:    nil, // scaled to zero: no running task
		nodes:    []swarm.Node{{ID: "nid1", Description: swarm.NodeDescription{Hostname: "worker-1"}}},
	}
	jobs, err := NewDiscoverer(f, "sabon", false, "sabon-cache", "", discardLog()).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Node != "worker-2" {
		t.Fatalf("scaled-to-0 service must pin via its node.hostname constraint; jobs=%+v", jobs)
	}
}

func TestSwarmDiscoverScaledToZeroUsesNodeLabel(t *testing.T) {
	svc := svcWith("cold2", "repo: cold2", []mount.Mount{{Type: mount.TypeVolume, Source: "d", Target: "/d"}})
	svc.Spec.TaskTemplate.Placement = &swarm.Placement{Constraints: []string{"node.labels.storage==true"}}
	f := &fakeSwarm{
		services: []swarm.Service{svc},
		nodes: []swarm.Node{
			{ID: "n1", Description: swarm.NodeDescription{Hostname: "plain"}},
			{ID: "n2", Spec: swarm.NodeSpec{Annotations: swarm.Annotations{Labels: map[string]string{"storage": "true"}}}, Description: swarm.NodeDescription{Hostname: "store"}},
		},
	}
	jobs, err := NewDiscoverer(f, "sabon", false, "sabon-cache", "", discardLog()).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Node != "store" {
		t.Fatalf("node.labels constraint must resolve to the labelled node; jobs=%+v", jobs)
	}
}

func TestSwarmDiscoverUnresolvedTaskFallsBackToConstraint(t *testing.T) {
	svc := svcWith("svc", "repo: app", []mount.Mount{{Type: mount.TypeVolume, Source: "v", Target: "/d"}})
	svc.Spec.TaskTemplate.Placement = &swarm.Placement{Constraints: []string{"node.hostname==worker-9"}}
	f := &fakeSwarm{
		services: []swarm.Service{svc},
		tasks:    []swarm.Task{{NodeID: "unknown", DesiredState: swarm.TaskStateRunning}}, // node not in NodeList
		nodes:    []swarm.Node{{ID: "nid1", Description: swarm.NodeDescription{Hostname: "worker-1"}}},
	}
	jobs, err := NewDiscoverer(f, "sabon", false, "sabon-cache", "", discardLog()).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(jobs) != 1 || jobs[0].Node != "worker-9" {
		t.Fatalf("an unresolved task node must fall back to the placement constraint (never a raw id); jobs=%+v", jobs)
	}
}
