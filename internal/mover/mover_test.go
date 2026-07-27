package mover

import (
	"context"
	"io"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

func TestParseResult(t *testing.T) {
	logs := "some restic output\n" +
		`SABON_RESULT {"snapshot_id":"abcd1234","files_new":5,"data_added_bytes":2048,"total_bytes_processed":4096,"backup_seconds":1.5}` + "\n" +
		"more output\n"
	r, ok := ParseResult(logs)
	if !ok {
		t.Fatal("ParseResult returned ok=false")
	}
	if r.SnapshotID != "abcd1234" {
		t.Errorf("SnapshotID = %q", r.SnapshotID)
	}
	if r.FilesNew != 5 {
		t.Errorf("FilesNew = %d", r.FilesNew)
	}
	if r.DataAddedBytes != 2048 {
		t.Errorf("DataAddedBytes = %d", r.DataAddedBytes)
	}
}

func TestParseResultAbsent(t *testing.T) {
	if _, ok := ParseResult("no marker here\n"); ok {
		t.Fatal("expected ok=false when no result marker present")
	}
}

func TestRunCmdPrependsGlobalArgs(t *testing.T) {
	var got []string
	orig := execRestic
	execRestic = func(_ context.Context, _ io.Writer, args ...string) error { got = args; return nil }
	defer func() { execRestic = orig; globalArgs = nil }()

	globalArgs = []string{"--limit-upload=1024", "-o", "s3.storage-class=IA"}
	_ = runCmd(context.Background(), io.Discard, "backup", "/data")

	want := []string{"--limit-upload=1024", "-o", "s3.storage-class=IA", "backup", "/data"}
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("args = %v, want %v", got, want)
		}
	}
}

func TestRunCmdNoGlobalArgs(t *testing.T) {
	var got []string
	orig := execRestic
	execRestic = func(_ context.Context, _ io.Writer, args ...string) error { got = args; return nil }
	defer func() { execRestic = orig; globalArgs = nil }()

	globalArgs = nil
	_ = runCmd(context.Background(), io.Discard, "check")
	if len(got) != 1 || got[0] != "check" {
		t.Fatalf("args = %v, want [check]", got)
	}
}

type fakeReapCli struct {
	client.APIClient
	list    []container.Summary
	removed []string
}

func (f *fakeReapCli) ContainerList(context.Context, container.ListOptions) ([]container.Summary, error) {
	return f.list, nil
}

func (f *fakeReapCli) ContainerRemove(_ context.Context, id string, _ container.RemoveOptions) error {
	f.removed = append(f.removed, id)
	return nil
}

func TestReapTrimsToN(t *testing.T) {
	bk := map[string]string{LabelKey: "a", LabelApp: "a", LabelTarget: "t", LabelAction: "backup"}
	sum := func(id, state string, created int64, labels map[string]string) container.Summary {
		return container.Summary{ID: id, State: state, Created: created, Labels: labels}
	}
	f := &fakeReapCli{list: []container.Summary{
		sum("b1", "exited", 100, bk),
		sum("b2", "exited", 200, bk),
		sum("b3", "exited", 300, bk),
		sum("b4", "exited", 400, bk),
		sum("run", "running", 500, bk),
		sum("stray", "exited", 50, map[string]string{LabelKey: "a", LabelApp: "a"}),
	}}
	n, err := (&Runner{cli: f}).Reap(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	removed := map[string]bool{}
	for _, id := range f.removed {
		removed[id] = true
	}
	for _, id := range []string{"b1", "b2", "stray"} {
		if !removed[id] {
			t.Errorf("expected %s removed; removed=%v", id, f.removed)
		}
	}
	for _, id := range []string{"b3", "b4", "run"} {
		if removed[id] {
			t.Errorf("expected %s kept; removed=%v", id, f.removed)
		}
	}
	if n != 3 {
		t.Errorf("removed count = %d, want 3", n)
	}
}
