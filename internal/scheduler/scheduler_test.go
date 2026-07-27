package scheduler

import (
	"io"
	"log/slog"
	"testing"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestSyncAddRemove(t *testing.T) {
	s := New(testLogger())
	noop := func() {}

	s.Sync([]Job{
		{Key: "a|onsite", Schedule: "0 0 * * * *", Run: noop},
		{Key: "b|onsite", Schedule: "0 30 * * * *", Run: noop},
	})
	if s.Len() != 2 {
		t.Fatalf("after add: Len = %d, want 2", s.Len())
	}

	// Drop b, keep a.
	s.Sync([]Job{{Key: "a|onsite", Schedule: "0 0 * * * *", Run: noop}})
	if s.Len() != 1 {
		t.Fatalf("after remove: Len = %d, want 1", s.Len())
	}
}

func TestSyncInvalidScheduleSkipped(t *testing.T) {
	s := New(testLogger())
	s.Sync([]Job{{Key: "bad", Schedule: "not a cron", Run: func() {}}})
	if s.Len() != 0 {
		t.Fatalf("invalid schedule should be skipped, Len = %d", s.Len())
	}
}

func TestSyncScheduleChangeReplaces(t *testing.T) {
	s := New(testLogger())
	noop := func() {}
	s.Sync([]Job{{Key: "a", Schedule: "0 0 * * * *", Run: noop}})
	s.Sync([]Job{{Key: "a", Schedule: "0 30 * * * *", Run: noop}})
	if s.Len() != 1 {
		t.Fatalf("Len = %d, want 1", s.Len())
	}
	if s.entries["a"].schedule != "0 30 * * * *" {
		t.Fatalf("schedule not updated: %q", s.entries["a"].schedule)
	}
}
