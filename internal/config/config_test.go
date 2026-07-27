package config

import "testing"

func TestLoadTargetsMissingFileOK(t *testing.T) {
	f, err := LoadTargets("/nonexistent/sabon/targets.yaml")
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(f.Targets) != 0 {
		t.Fatalf("expected 0 targets, got %d", len(f.Targets))
	}
}

func TestLoadSnapshotMode(t *testing.T) {
	t.Setenv("SABON_CONFIG", "/nonexistent/sabon/targets.yaml")
	for _, m := range []string{"none", "zfs", "auto"} {
		t.Run(m, func(t *testing.T) {
			t.Setenv("SABON_SNAPSHOT", m)
			if _, err := Load(); err != nil {
				t.Errorf("Load with SABON_SNAPSHOT=%s: %v", m, err)
			}
		})
	}
	t.Run("invalid", func(t *testing.T) {
		t.Setenv("SABON_SNAPSHOT", "btrfs")
		if _, err := Load(); err == nil {
			t.Error("Load must reject an unknown snapshot mode")
		}
	})
}
