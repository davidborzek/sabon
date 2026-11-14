package mover

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// guardDestination runs in the mover's filesystem, before even unlock (which
// writes to the repository). It never provisions directories or marker files.
func guardDestination(spec Spec) error {
	if spec.DestinationMarker != "" {
		repo := os.Getenv("RESTIC_REPOSITORY")
		if !filepath.IsAbs(repo) {
			return fmt.Errorf("destination marker requires an absolute local repository path")
		}
		marker := filepath.Join(repo, ".sabon-destination")
		info, err := os.Lstat(marker)
		if err != nil {
			return fmt.Errorf("destination marker: %w", err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("destination marker must be a regular file (not a symlink)")
		}
		// Read at most the expected identity plus an optional final newline and
		// one extra byte to detect trailing data, regardless of file size.
		f, err := os.Open(marker)
		if err != nil {
			return fmt.Errorf("destination marker: %w", err)
		}
		defer func() { _ = f.Close() }()
		data, err := io.ReadAll(io.LimitReader(f, int64(len(spec.DestinationMarker))+2))
		if err != nil {
			return fmt.Errorf("destination marker: %w", err)
		}
		if string(data) != spec.DestinationMarker && string(data) != spec.DestinationMarker+"\n" {
			return fmt.Errorf("destination marker identity mismatch")
		}
	}
	return nil
}
