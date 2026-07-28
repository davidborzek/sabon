package docker

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
)

// fakeDocker satisfies client.APIClient by embedding it; only the methods used
// by discovery are implemented (others panic if ever called).
type fakeDocker struct {
	client.APIClient
	list    []container.Summary
	mounts  map[string][]container.MountPoint
	gotOpts container.ListOptions
}

func (f *fakeDocker) ContainerList(_ context.Context, opts container.ListOptions) ([]container.Summary, error) {
	f.gotOpts = opts
	return f.list, nil
}

func (f *fakeDocker) ContainerInspect(_ context.Context, name string) (container.InspectResponse, error) {
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{Name: "/" + name},
		Mounts:            f.mounts[name],
	}, nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestListResolvesSources(t *testing.T) {
	doc := "repo: immich\nextraVolumes: [immich-db-dump]\nextraPaths: [/srv/extra]\n"
	fd := &fakeDocker{
		list: []container.Summary{{
			ID:    "abc",
			Names: []string{"/immich-server"},
			Labels: map[string]string{
				"sabon.enable": "true",
				"sabon.backup": doc,
			},
		}},
		mounts: map[string][]container.MountPoint{
			"immich-server": {
				{Type: mount.TypeBind, Source: "/mnt/data/immich", Destination: "/data"},
				{Type: mount.TypeVolume, Name: "immich-model-cache", Destination: "/cache-models"},
				{Type: mount.TypeVolume, Name: "sabon-cache", Destination: "/cache"}, // must be skipped
			},
		},
	}
	d := NewDiscoverer(fd, "sabon", false, "sabon-cache", "", testLogger())
	jobs, err := d.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("want 1 job, got %d", len(jobs))
	}
	j := jobs[0]
	if j.App != "immich" {
		t.Errorf("App = %q, want immich", j.App)
	}
	// auto: /data (bind) + immich-model-cache (volume); sabon-cache skipped.
	// explicit: immich-db-dump (volume) + /srv/extra (bind) => 4 total.
	if len(j.Sources) != 4 {
		t.Fatalf("want 4 sources, got %d: %+v", len(j.Sources), j.Sources)
	}
	names := map[string]bool{}
	for _, s := range j.Sources {
		if names[s.Name] {
			t.Errorf("duplicate source name %q", s.Name)
		}
		names[s.Name] = true
		if s.Ref == "sabon-cache" {
			t.Error("cache volume must not be a source")
		}
	}
	if !names["data"] || !names["immich-model-cache"] || !names["immich-db-dump"] || !names["extra"] {
		t.Errorf("unexpected source names: %v", names)
	}
}

func TestListSkipsDisabledAndSpecless(t *testing.T) {
	fd := &fakeDocker{
		list: []container.Summary{
			{Names: []string{"/no-labels"}, Labels: map[string]string{}},
			{Names: []string{"/enabled-no-spec"}, Labels: map[string]string{"sabon.enable": "true"}},
			{Names: []string{"/spec-but-disabled"}, Labels: map[string]string{"sabon.enable": "false", "sabon.backup": "repo: x\n"}},
		},
		mounts: map[string][]container.MountPoint{},
	}
	d := NewDiscoverer(fd, "sabon", false, "sabon-cache", "", testLogger())
	jobs, err := d.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("want 0 jobs, got %d", len(jobs))
	}
}

func TestListInstanceFilter(t *testing.T) {
	spec := "auto: false\nextraPaths: [/srv/x]\n"
	fd := &fakeDocker{
		list: []container.Summary{
			{Names: []string{"/mine"}, Labels: map[string]string{"sabon.enable": "true", "sabon.instance": "a", "sabon.backup": "repo: mine\n" + spec}},
			{Names: []string{"/theirs"}, Labels: map[string]string{"sabon.enable": "true", "sabon.instance": "b", "sabon.backup": "repo: theirs\n" + spec}},
			{Names: []string{"/unowned"}, Labels: map[string]string{"sabon.enable": "true", "sabon.backup": "repo: unowned\n" + spec}},
		},
		mounts: map[string][]container.MountPoint{},
	}
	// instance "a" → only the container tagged a.
	d := NewDiscoverer(fd, "sabon", false, "sabon-cache", "a", testLogger())
	jobs, err := d.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(jobs) != 1 || jobs[0].App != "mine" {
		t.Fatalf("instance filter: want [mine], got %+v", jobs)
	}
	// no instance → manage all three.
	d2 := NewDiscoverer(fd, "sabon", false, "sabon-cache", "", testLogger())
	jobs2, _ := d2.List(context.Background())
	if len(jobs2) != 3 {
		t.Fatalf("no instance: want 3 jobs, got %d", len(jobs2))
	}
}

func TestListIncludesStopped(t *testing.T) {
	fd := &fakeDocker{
		list: []container.Summary{{
			Names:  []string{"/db"},
			Labels: map[string]string{"sabon.enable": "true", "sabon.backup": "repo: db\nauto: false\nextraPaths: [/srv/db]\n"},
		}},
		mounts: map[string][]container.MountPoint{},
	}
	d := NewDiscoverer(fd, "sabon", false, "sabon-cache", "", testLogger())
	if _, err := d.List(context.Background()); err != nil {
		t.Fatalf("List: %v", err)
	}
	if !fd.gotOpts.All {
		t.Error("ContainerList must use All=true so stopped containers are still backed up")
	}
}

func TestListExcludeVolumes(t *testing.T) {
	doc := "repo: immich\nexcludeVolumes: [immich-model-cache]\n"
	fd := &fakeDocker{
		list: []container.Summary{{
			ID:     "abc",
			Names:  []string{"/immich-server"},
			Labels: map[string]string{"sabon.enable": "true", "sabon.backup": doc},
		}},
		mounts: map[string][]container.MountPoint{
			"immich-server": {
				{Type: mount.TypeBind, Source: "/mnt/data/immich", Destination: "/data"},
				{Type: mount.TypeVolume, Name: "immich-library", Destination: "/lib"},
				{Type: mount.TypeVolume, Name: "immich-model-cache", Destination: "/cache-models"}, // excluded
			},
		},
	}
	d := NewDiscoverer(fd, "sabon", false, "sabon-cache", "", testLogger())
	jobs, err := d.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("want 1 job, got %d", len(jobs))
	}
	names := map[string]bool{}
	for _, s := range jobs[0].Sources {
		names[s.Name] = true
		if s.Ref == "immich-model-cache" {
			t.Error("excludeVolumes volume must not be a source")
		}
	}
	// auto: /data (bind) + immich-library (volume); model-cache excluded => 2.
	if len(jobs[0].Sources) != 2 || !names["data"] || !names["immich-library"] {
		t.Errorf("want [data immich-library], got %v", names)
	}
}
