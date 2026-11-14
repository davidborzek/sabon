package mover

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestDestinationMarker(t *testing.T) {
	// No restic call may precede destination validation.
	for _, tc := range []struct {
		name, content, action string
		valid                 bool
	}{
		{"missing", "", ActionBackup, false},
		{"wrong", "other", ActionRestore, false},
		{"directory", "", ActionSnapshots, false},
		{"symlink", "", ActionCheck, false},
		{"trailing-data", "disk\nextra", ActionPrune, false},
		{"exact", "disk", "", true},
		{"newline", "disk\n", ActionBackup, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			t.Setenv("RESTIC_REPOSITORY", repo)
			marker := filepath.Join(repo, ".sabon-destination")
			var err error
			switch tc.name {
			case "directory":
				err = os.Mkdir(marker, 0700)
			case "symlink":
				err = os.Symlink("missing", marker)
			default:
				if tc.content != "" {
					err = os.WriteFile(marker, []byte(tc.content), 0600)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			original := execRestic
			t.Cleanup(func() { execRestic = original; globalArgs = nil })
			var calls []string
			execRestic = func(_ context.Context, _ io.Writer, args ...string) error {
				if !tc.valid {
					t.Fatal("restic invoked before destination validation")
				}
				calls = append(calls, args[0])
				if args[0] == "cat" {
					return os.ErrNotExist
				}
				return nil
			}
			raw, err := json.Marshal(Spec{App: "demo", Sources: []string{"/data"}, Action: tc.action, DestinationMarker: "disk"})
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv(SpecEnv, string(raw))
			if err := Execute(context.Background(), io.Discard, io.Discard); (err == nil) != tc.valid {
				t.Fatalf("Execute() = %v, want valid=%v", err, tc.valid)
			}
			if tc.valid && (!slices.Contains(calls, "init") || !slices.Contains(calls, "backup")) {
				t.Fatalf("valid marker must allow initialization and backup, got %v", calls)
			}
			if tc.name == "missing" {
				entries, err := os.ReadDir(repo)
				if err != nil || len(entries) != 0 {
					t.Fatalf("destination modified: %v %v", entries, err)
				}
			}
		})
	}
}
