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

func TestLoadAPITokenOptional(t *testing.T) {
	t.Setenv("SABON_CONFIG", "/nonexistent/sabon/targets.yaml")
	t.Setenv("SABON_API_ADDR", ":8080")
	t.Run("without token", func(t *testing.T) {
		t.Setenv("SABON_API_TOKEN", "")
		if _, err := Load(); err != nil {
			t.Errorf("API token is optional; Load must not fail without it: %v", err)
		}
	})
	t.Run("with token", func(t *testing.T) {
		t.Setenv("SABON_API_TOKEN", "secret")
		if _, err := Load(); err != nil {
			t.Errorf("Load with addr+token: %v", err)
		}
	})
}

func TestLoadMoverLabels(t *testing.T) {
	t.Setenv("SABON_CONFIG", "/nonexistent/sabon/targets.yaml")
	t.Run("parses key=value pairs", func(t *testing.T) {
		t.Setenv("SABON_MOVER_LABELS", "team=infra, cost-center=backups")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.MoverLabels["team"] != "infra" || c.MoverLabels["cost-center"] != "backups" {
			t.Errorf("MoverLabels = %v", c.MoverLabels)
		}
	})
	t.Run("rejects non-pair entries", func(t *testing.T) {
		t.Setenv("SABON_MOVER_LABELS", "team")
		if _, err := Load(); err == nil {
			t.Error("Load must reject an entry without =")
		}
	})
	t.Run("rejects reserved sabon. prefix", func(t *testing.T) {
		t.Setenv("SABON_MOVER_LABELS", "sabon.app=evil")
		if _, err := Load(); err == nil {
			t.Error("Load must reject the reserved sabon. prefix")
		}
	})
}

func TestHealthAddrFromEnv(t *testing.T) {
	t.Run("metrics enabled: no extra listener", func(t *testing.T) {
		t.Setenv("SABON_METRICS_ADDR", ":9333")
		if got := HealthAddrFromEnv(); got != "" {
			t.Errorf("HealthAddrFromEnv() = %q, want \"\"", got)
		}
		if got := HealthProbeAddr(); got != ":9333" {
			t.Errorf("HealthProbeAddr() = %q, want \":9333\"", got)
		}
	})

	t.Run("metrics disabled: loopback listener", func(t *testing.T) {
		t.Setenv("SABON_METRICS_ADDR", "")
		if got := HealthAddrFromEnv(); got != DefaultHealthAddr {
			t.Errorf("HealthAddrFromEnv() = %q, want %q", got, DefaultHealthAddr)
		}
		if got := HealthProbeAddr(); got != DefaultHealthAddr {
			t.Errorf("HealthProbeAddr() = %q, want %q", got, DefaultHealthAddr)
		}
	})

	t.Run("override wins over the metrics server", func(t *testing.T) {
		t.Setenv("SABON_METRICS_ADDR", ":9333")
		t.Setenv("SABON_HEALTH_ADDR", "127.0.0.1:9999")
		if got := HealthProbeAddr(); got != "127.0.0.1:9999" {
			t.Errorf("HealthProbeAddr() = %q, want \"127.0.0.1:9999\"", got)
		}
	})

	t.Run("both blank: nothing to probe", func(t *testing.T) {
		t.Setenv("SABON_METRICS_ADDR", "")
		t.Setenv("SABON_HEALTH_ADDR", "")
		if got := HealthProbeAddr(); got != "" {
			t.Errorf("HealthProbeAddr() = %q, want \"\"", got)
		}
	})
}
