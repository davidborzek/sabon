package mover

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/davidborzek/sabon/api"
)

// A lock held by another client must survive every mover entrypoint.
func TestExecutePreservesForeignLocks(t *testing.T) {
	for _, action := range []string{"", ActionBackup, ActionRestore, ActionSnapshots, ActionCheck, ActionPrune} {
		t.Run(action, func(t *testing.T) {
			locked := true
			lockErr := errors.New("repository is already locked")
			var calls []string
			orig := execRestic
			t.Cleanup(func() { execRestic = orig; globalArgs = nil })
			execRestic = func(_ context.Context, _ io.Writer, args ...string) error {
				calls = append(calls, args[0])
				switch args[0] {
				case "cat":
					return nil // Existing repository; startup must not reinitialise it.
				case "unlock":
					locked = false
					return nil
				default:
					if locked {
						return lockErr
					}
					return nil
				}
			}
			spec := Spec{App: "fixture", Action: action, Sources: []string{"/fixture"}, RestoreTarget: "/restore", Retention: api.Retention{Last: 1}}
			raw, err := MarshalSpec(spec)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv(SpecEnv, raw)
			err = Execute(context.Background(), io.Discard, io.Discard)
			if !errors.Is(err, lockErr) || !locked {
				t.Fatalf("foreign lock must survive and conflict must propagate: locked=%v err=%v calls=%v", locked, err, calls)
			}
			want := []string{action}
			if action == "" || action == ActionBackup {
				want = []string{"cat", "backup"}
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("calls=%v, want %v (no cleanup, retries, or retention after conflict)", calls, want)
			}
		})
	}
}

func TestRetentionPreservesLockAcquiredAfterBackup(t *testing.T) {
	lockErr := errors.New("repository is already locked")
	var calls []string
	orig := execRestic
	t.Cleanup(func() { execRestic = orig; globalArgs = nil })
	execRestic = func(_ context.Context, _ io.Writer, args ...string) error {
		calls = append(calls, args[0])
		if args[0] == "forget" {
			return lockErr // Another client acquired a lock between the two commands.
		}
		return nil
	}
	raw, err := MarshalSpec(Spec{App: "fixture", Sources: []string{"/fixture"}, Retention: api.Retention{Last: 1}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(SpecEnv, raw)
	if err := Execute(context.Background(), io.Discard, io.Discard); !errors.Is(err, lockErr) {
		t.Fatalf("retention conflict must propagate, got %v", err)
	}
	if want := []string{"cat", "backup", "forget"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%v, want %v", calls, want)
	}
}
