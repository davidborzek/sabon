package main

import (
	"testing"

	"github.com/davidborzek/sabon/internal/config"
)

func TestMoverImage(t *testing.T) {
	old := version
	defer func() { version = old }()

	version = "1.2.3"
	if got, err := moverImage(&config.Config{MoverImage: "custom:tag"}); err != nil || got != "custom:tag" {
		t.Errorf("configured image: got %q, %v", got, err)
	}
	if got, err := moverImage(&config.Config{}); err != nil || got != "ghcr.io/davidborzek/sabon/mover:1.2.3" {
		t.Errorf("version-matched default: got %q, %v", got, err)
	}

	// A dev build has no published mover image to match: fail closed.
	version = "dev"
	if _, err := moverImage(&config.Config{}); err == nil {
		t.Error("dev build without SABON_MOVER_IMAGE should fail")
	}
	if got, err := moverImage(&config.Config{MoverImage: "x:1"}); err != nil || got != "x:1" {
		t.Errorf("configured image on dev: got %q, %v", got, err)
	}
}

func TestMoverImageMigratesLegacyOrchestratorImage(t *testing.T) {
	old := version
	defer func() { version = old }()
	version = "1.2.3"

	tests := []struct {
		configured, want string
		migrated         bool
	}{
		{"ghcr.io/davidborzek/sabon:0.3.2", "ghcr.io/davidborzek/sabon/mover:0.3.2", true},
		{"ghcr.io/davidborzek/sabon:latest", "ghcr.io/davidborzek/sabon/mover:latest", true},
		{"ghcr.io/davidborzek/sabon:main", "ghcr.io/davidborzek/sabon/mover:main", true},
		{"ghcr.io/davidborzek/sabon", "ghcr.io/davidborzek/sabon/mover:latest", true},
		{"ghcr.io/davidborzek/sabon:0.3.2@sha256:abc", "ghcr.io/davidborzek/sabon/mover:0.3.2", true},
		// A digest names one orchestrator build; the mover has its own digest.
		{"ghcr.io/davidborzek/sabon@sha256:abc", "ghcr.io/davidborzek/sabon/mover:1.2.3", true},
		{"ghcr.io/davidborzek/sabon/mover:0.3.2", "ghcr.io/davidborzek/sabon/mover:0.3.2", false},
		{"ghcr.io/davidborzek/sabon-fork:1", "ghcr.io/davidborzek/sabon-fork:1", false},
		{"registry.example/sabon:1", "registry.example/sabon:1", false},
	}
	for _, tt := range tests {
		got, migrated, err := migratedMoverImage(tt.configured)
		if err != nil || got != tt.want || migrated != tt.migrated {
			t.Errorf("migratedMoverImage(%q) = %q, %v, %v; want %q, %v", tt.configured, got, migrated, err, tt.want, tt.migrated)
		}
	}

	// A digest-only legacy ref on a dev build has no version to fall back to.
	version = "dev"
	if _, _, err := migratedMoverImage("ghcr.io/davidborzek/sabon@sha256:abc"); err == nil {
		t.Error("digest-only legacy ref on a dev build should fail")
	}
}

func TestZFSSnapshotterImage(t *testing.T) {
	old := version
	defer func() { version = old }()

	// An explicit configuration always wins.
	version = "v1.2.3"
	if got := zfsSnapshotterImage(&config.Config{SnapshotZFSImage: "custom:tag"}); got != "custom:tag" {
		t.Errorf("configured image: got %q, want custom:tag", got)
	}

	// Unset resolves to the tag matching this binary's version.
	if got := zfsSnapshotterImage(&config.Config{}); got != "ghcr.io/davidborzek/sabon/zfs-snapshotter:v1.2.3" {
		t.Errorf("version-matched default: got %q", got)
	}

	// A dev build (no version stamped in) fails closed: no image, so ZFS
	// snapshots require an explicit SABON_SNAPSHOT_ZFS_IMAGE rather than a
	// floating :latest privileged image.
	version = "dev"
	if got := zfsSnapshotterImage(&config.Config{}); got != "" {
		t.Errorf("dev build should return empty image, got %q", got)
	}
	if got := zfsSnapshotterImage(&config.Config{SnapshotZFSImage: "x:1"}); got != "x:1" {
		t.Errorf("configured image still wins on dev: got %q", got)
	}
}
