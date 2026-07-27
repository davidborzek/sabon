package main

import (
	"testing"

	"github.com/davidborzek/sabon/internal/config"
)

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
