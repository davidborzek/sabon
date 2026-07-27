//go:build e2e

// Package e2e exercises sabon end to end against a real Docker daemon: it builds
// the sabon image and binary, labels a demo container, runs a backup through a
// spawned mover, then lists and restores the snapshot. Run with:
//
//	go test -tags e2e ./test/e2e/
//
// It skips when Docker is unavailable.
package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	image    = "sabon:e2e"
	app      = "e2eapp"
	password = "e2e-secret"
)

func run(t *testing.T, name string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func mustRun(t *testing.T, env []string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func TestBackupSnapshotsRestore(t *testing.T) {
	if _, err := run(t, "docker", "version"); err != nil {
		t.Skip("docker not available")
	}

	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}

	// Build the sabon image (bundles restic; also used as the mover image) and
	// the sabon binary that acts as the orchestrator in this test.
	if out, err := run(t, "docker", "build", "-t", image, "--build-arg", "VERSION=e2e", repoRoot); err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
	bin := filepath.Join(t.TempDir(), "sabon")
	if out, err := run(t, "go", "build", "-o", bin, filepath.Join(repoRoot, "cmd", "sabon")); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	// Not t.TempDir: the mover runs as root and leaves root-owned files in the
	// repo and restore dirs, which the test user cannot remove. Clean up via a
	// throwaway root container instead.
	work, err := os.MkdirTemp("", "sabon-e2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = run(t, "docker", "run", "--rm", "-v", work+":/w", "alpine:3", "sh", "-c", "rm -rf /w/repo /w/restore")
		_ = os.RemoveAll(work)
	})
	src := filepath.Join(work, "src")
	repo := filepath.Join(work, "repo")
	restore := filepath.Join(work, "restore")
	if err := os.MkdirAll(src, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(repo, 0o777); err != nil {
		t.Fatal(err)
	}
	payload := "e2e payload " + time.Now().Format(time.RFC3339Nano)
	if err := os.WriteFile(filepath.Join(src, "data.txt"), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	targets := filepath.Join(work, "targets.yaml")
	if err := os.WriteFile(targets, []byte("targets:\n  - name: onsite\n    path: "+repo+"\n    schedule: \"0 0 * * * *\"\n    retention:\n      last: 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A labelled demo workload.
	container := "sabon-e2e-app"
	_, _ = run(t, "docker", "rm", "-f", container)
	spec := "repo: " + app + "\nauto: false\nextraPaths: [" + src + "]\n"
	if out, err := run(t, "docker", "run", "-d", "--name", container,
		"-v", src+":/data",
		"--label", "sabon.enable=true",
		"--label", "sabon.backup="+spec,
		"alpine:3", "sleep", "600"); err != nil {
		t.Fatalf("docker run demo: %v\n%s", err, out)
	}
	t.Cleanup(func() { _, _ = run(t, "docker", "rm", "-f", container) })

	env := []string{
		"SABON_CONFIG=" + targets,
		"SABON_MOVER_IMAGE=" + image,
		"RESTIC_PASSWORD=" + password,
	}

	// Back up.
	out := mustRun(t, env, bin, "backup")
	if !strings.Contains(out, "backup complete") {
		t.Fatalf("backup did not complete:\n%s", out)
	}

	// List snapshots.
	out = mustRun(t, env, bin, "snapshots", "--app", app, "--target", "onsite")
	if !strings.Contains(out, app) {
		t.Fatalf("snapshots did not list the app:\n%s", out)
	}

	// Restore into a staging dir and verify the payload round-trips.
	mustRun(t, env, bin, "restore", "--app", app, "--target", "onsite", "--into", restore)
	got, err := os.ReadFile(filepath.Join(restore, strings.TrimPrefix(src, "/"), "data.txt"))
	if err != nil {
		// restic restores absolute source paths under the target; locate the file.
		var found string
		_ = filepath.Walk(restore, func(p string, info os.FileInfo, _ error) error {
			if info != nil && !info.IsDir() && filepath.Base(p) == "data.txt" {
				found = p
			}
			return nil
		})
		if found == "" {
			t.Fatalf("restored data.txt not found under %s", restore)
		}
		got, err = os.ReadFile(found)
		if err != nil {
			t.Fatal(err)
		}
	}
	if string(got) != payload {
		t.Fatalf("restored payload mismatch: got %q want %q", got, payload)
	}
}
