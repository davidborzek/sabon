//go:build e2e

// Swarm end-to-end: on a single-node `docker swarm init` cluster, sabon runs in
// swarm mode, discovers a labelled *service*, spawns the mover as a one-shot
// node-pinned service, then lists and restores the snapshot. This is the gate
// for the otherwise cluster-unvalidated swarm runtime. Run with:
//
//	go test -tags e2e -run Swarm ./test/e2e/
//
// It skips when Docker is unavailable or the host cannot init a swarm.
package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// waitSwarmService blocks until the service has a task in the Running state, so
// discovery can resolve the node it sits on.
func waitSwarmService(t *testing.T, name string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		out, _ := run(t, "docker", "service", "ps", name, "--filter", "desired-state=running", "--format", "{{.CurrentState}}")
		if strings.Contains(out, "Running") {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("service %s did not reach Running in time", name)
}

func TestSwarmBackupSnapshotsRestore(t *testing.T) {
	if _, err := run(t, "docker", "version"); err != nil {
		t.Skip("docker not available")
	}
	// Single-node swarm. If the host is already a swarm (e.g. a dev box), reuse
	// it and do NOT tear it down; otherwise init one and leave on cleanup.
	if out, err := run(t, "docker", "swarm", "init", "--advertise-addr", "127.0.0.1"); err != nil {
		if !strings.Contains(out, "already part of a swarm") {
			t.Skipf("cannot init a swarm on this host: %v\n%s", err, out)
		}
	} else {
		t.Cleanup(func() { _, _ = run(t, "docker", "swarm", "leave", "--force") })
	}

	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	// sabon image (mover) + the orchestrator binary. docker build caches, so
	// sharing the layers with the standalone e2e is cheap.
	if out, err := run(t, "docker", "build", "-t", image, "--build-arg", "VERSION=e2e", repoRoot); err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
	bin := filepath.Join(t.TempDir(), "sabon")
	if out, err := run(t, "go", "build", "-o", bin, filepath.Join(repoRoot, "cmd", "sabon")); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	// Not t.TempDir: the mover runs as root and leaves root-owned files behind.
	work, err := os.MkdirTemp("", "sabon-e2e-swarm")
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
	for _, d := range []string{src, repo} {
		if err := os.MkdirAll(d, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	payload := "e2e swarm payload " + time.Now().Format(time.RFC3339Nano)
	if err := os.WriteFile(filepath.Join(src, "data.txt"), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	targets := filepath.Join(work, "targets.yaml")
	if err := os.WriteFile(targets, []byte("targets:\n  - name: onsite\n    path: "+repo+"\n    schedule: \"0 0 * * * *\"\n    retention:\n      last: 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A labelled demo *service* — in swarm the labels sit on the service, and
	// `--label` on `service create` sets exactly those service labels.
	svc := "sabon-e2e-swarm-app"
	_, _ = run(t, "docker", "service", "rm", svc)
	spec := "repo: " + app + "\nauto: false\nextraPaths: [" + src + "]\n"
	if out, err := run(t, "docker", "service", "create", "--name", svc,
		"--replicas", "1",
		"--label", "sabon.enable=true",
		"--label", "sabon.backup="+spec,
		"alpine:3", "sleep", "600"); err != nil {
		t.Fatalf("service create: %v\n%s", err, out)
	}
	t.Cleanup(func() { _, _ = run(t, "docker", "service", "rm", svc) })
	waitSwarmService(t, svc)

	env := []string{
		"SABON_CONFIG=" + targets,
		"SABON_RUNTIME=swarm",
		"SABON_MOVER_IMAGE=" + image,
		"RESTIC_PASSWORD=" + password,
	}

	// Back up: the mover runs as a one-shot node-pinned service.
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
	got, err := os.ReadFile(found)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != payload {
		t.Fatalf("restored payload mismatch: got %q want %q", got, payload)
	}
}
