package zfs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/davidborzek/sabon/internal/snapshot"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestParseDatasets(t *testing.T) {
	out := "tank\t/tank\ntank/data\t/mnt/data\ntank/legacy\tlegacy\ntank/nomnt\t-\ntank/off\tnone\n"
	ds := parseDatasets(out)
	if len(ds) != 2 {
		t.Fatalf("want 2 datasets (mounted only), got %d: %+v", len(ds), ds)
	}
	if ds[1].name != "tank/data" || ds[1].mountpoint != "/mnt/data" {
		t.Errorf("unexpected second dataset: %+v", ds[1])
	}
}

func TestDatasetFor(t *testing.T) {
	ds := []dataset{
		{"tank", "/"},
		{"tank/data", "/mnt/data"},
		{"tank/data/immich", "/mnt/data/immich"}, // child dataset of tank/data
		{"tank/vols", "/var/lib/docker/volumes"},
	}
	cases := []struct {
		path, wantDS, wantSub string
		ok                    bool
	}{
		{"/mnt/data/paperless", "tank/data", "paperless", true},
		{"/mnt/data/immich/photos", "tank/data/immich", "photos", true}, // prefers the child dataset over its parent
		{"/mnt/data/immich", "tank/data/immich", "", true},
		{"/mnt/data", "tank/data", "", true},
		{"/var/lib/docker/volumes/immich-db/_data", "tank/vols", "immich-db/_data", true},
		{"/etc/hosts", "tank", "etc/hosts", true}, // longest-prefix falls back to the root dataset
	}
	for _, c := range cases {
		d, sub, ok := datasetFor(c.path, ds)
		if ok != c.ok || d.name != c.wantDS || sub != c.wantSub {
			t.Errorf("datasetFor(%q) = %q,%q,%v; want %q,%q,%v", c.path, d.name, sub, ok, c.wantDS, c.wantSub, c.ok)
		}
	}
	if _, _, ok := datasetFor("/etc/hosts", []dataset{{"tank/data", "/mnt/data"}}); ok {
		t.Error("expected not-found when no dataset contains the path")
	}
}

func TestReadPath(t *testing.T) {
	if got := readPath("/mnt/data", "sabon-x-1", "immich"); got != "/mnt/data/.zfs/snapshot/sabon-x-1/immich" {
		t.Errorf("readPath = %q", got)
	}
	if got := readPath("/mnt/data", "sabon-x-1", ""); got != "/mnt/data/.zfs/snapshot/sabon-x-1" {
		t.Errorf("readPath empty sub = %q", got)
	}
}

func TestSanitize(t *testing.T) {
	if got := sanitize("im/mich:1"); got != "im-mich:1" {
		t.Errorf("sanitize = %q, want im-mich:1", got)
	}
}

func TestSnapshotFlow(t *testing.T) {
	var cmds [][]string
	run := func(_ context.Context, args ...string) (string, error) {
		cmds = append(cmds, args)
		if args[0] == "list" {
			return "tank/data\t/mnt/data\ntank/other\t/mnt/other\n", nil
		}
		return "", nil
	}
	z := &snapshotter{runner: runnerFunc(run), log: testLog()}
	srcs := []snapshot.Source{
		{Name: "a", HostPath: "/mnt/data/immich"},
		{Name: "b", HostPath: "/mnt/data/paperless"}, // same dataset -> reuse snapshot
		{Name: "c", HostPath: "/mnt/other/x"},        // different dataset
	}
	mounts, cleanup, err := z.Snapshot(context.Background(), "immich", srcs)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(mounts) != 3 {
		t.Fatalf("want 3 mounts, got %d", len(mounts))
	}
	// One atomic snapshot command covering both distinct datasets (the shared
	// dataset is deduped): args are [snapshot, tank/data@…, tank/other@…].
	snap := find(cmds, "snapshot")
	if snap == nil || len(snap) != 3 {
		t.Errorf("want one snapshot command with 2 dataset snapshots, got %v", cmds)
	}
	if !strings.Contains(mounts[0].HostPath, "/mnt/data/.zfs/snapshot/") || !strings.HasSuffix(mounts[0].HostPath, "/immich") {
		t.Errorf("mount a read path = %q", mounts[0].HostPath)
	}
	cleanup(context.Background())
	if n := count(cmds, "destroy"); n != 2 {
		t.Errorf("want 2 destroy commands (one per dataset), got %d", n)
	}
}

func TestSnapshotNonZFSFails(t *testing.T) {
	var cmds [][]string
	run := func(_ context.Context, args ...string) (string, error) {
		cmds = append(cmds, args)
		if args[0] == "list" {
			return "tank/data\t/mnt/data\n", nil
		}
		return "", nil
	}
	z := &snapshotter{runner: runnerFunc(run), log: testLog()}
	_, _, err := z.Snapshot(context.Background(), "app", []snapshot.Source{
		{Name: "ok", HostPath: "/mnt/data/x"},
		{Name: "bad", HostPath: "/opt/other"}, // not on any dataset -> hard fail
	})
	if err == nil {
		t.Fatal("expected error for a source not on a ZFS dataset")
	}
	if n := count(cmds, "snapshot"); n != 0 {
		t.Errorf("no snapshot must be taken when a source is not on ZFS, got %d (%v)", n, cmds)
	}
}

// runnerFunc adapts a function to the runner interface for tests.
type runnerFunc func(ctx context.Context, args ...string) (string, error)

func (f runnerFunc) run(ctx context.Context, args ...string) (string, error) {
	return f(ctx, args...)
}

func (runnerFunc) reap(context.Context) (int, error) { return 0, nil }

func TestReapDestroysOnlyOwnInstanceSnapshots(t *testing.T) {
	var cmds [][]string
	run := func(_ context.Context, args ...string) (string, error) {
		cmds = append(cmds, args)
		if args[0] == "list" {
			return "tank/data@sabon-prod-immich-1\n" + // ours
				"tank/data@sabon-staging-immich-2\n" + // sibling instance
				"tank/other@sabon-prod-paperless-3\n" + // ours
				"tank/data@manual-keep\n", nil // not sabon
		}
		return "", nil
	}
	z := &snapshotter{runner: runnerFunc(run), instance: "prod", log: testLog()}
	// immich's snapshot is still mounted into a running mover -> must be spared.
	inUse := []string{"/mnt/data/.zfs/snapshot/sabon-prod-immich-1/immich"}
	n, err := z.Reap(context.Background(), inUse)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if n != 1 {
		t.Errorf("want 1 snapshot reaped (only the orphan paperless), got %d", n)
	}
	for _, c := range cmds {
		if c[0] != "destroy" {
			continue
		}
		snap := c[1][strings.Index(c[1], "@")+1:]
		if !strings.HasPrefix(snap, "sabon-prod-") {
			t.Errorf("reaped a snapshot outside this instance: %v", c)
		}
		if snap == "sabon-prod-immich-1" {
			t.Errorf("reaped an in-use snapshot: %v", c)
		}
	}
}

func count(cmds [][]string, verb string) int {
	n := 0
	for _, c := range cmds {
		if len(c) > 0 && c[0] == verb {
			n++
		}
	}
	return n
}

func find(cmds [][]string, verb string) []string {
	for _, c := range cmds {
		if len(c) > 0 && c[0] == verb {
			return c
		}
	}
	return nil
}

func TestResolve(t *testing.T) {
	run := func(_ context.Context, args ...string) (string, error) {
		if args[0] == "list" {
			return "tank/data\t/mnt/data\n", nil
		}
		return "", nil
	}
	res, err := (&snapshotter{runner: runnerFunc(run), log: testLog()}).Resolve(context.Background(), []snapshot.Source{
		{Name: "a", HostPath: "/mnt/data/immich"}, // on ZFS
		{Name: "b", HostPath: "/opt/other"},       // not on ZFS
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res[0].Name != "a" || !res[0].Snapshottable || res[0].Detail != "dataset tank/data" {
		t.Errorf("source a: got %+v, want name=a snapshottable=true detail=\"dataset tank/data\"", res[0])
	}
	if res[1].Name != "b" || res[1].Snapshottable {
		t.Errorf("source b: got %+v, want name=b not snapshottable", res[1])
	}
	// a failing `zfs list` means zfs is unavailable on the host.
	bad := func(context.Context, ...string) (string, error) { return "", errors.New("no zfs") }
	if _, err := (&snapshotter{runner: runnerFunc(bad), log: testLog()}).Resolve(context.Background(), nil); err == nil {
		t.Error("expected error when zfs is unavailable")
	}
}
