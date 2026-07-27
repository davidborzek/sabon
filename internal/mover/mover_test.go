package mover

import (
	"context"
	"io"
	"testing"
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
