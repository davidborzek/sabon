// Package mover defines the contract between sabon's orchestrator and the
// ephemeral "mover" containers it spawns, and implements the mover side: a
// self-contained restic run (init-if-needed -> backup -> forget/prune) executed
// inside a throwaway container that has the app's volumes mounted.
//
// The orchestrator half (spawning, waiting, reaping) lives in runner.go.
package mover

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/davidborzek/sabon/api"
)

// SpecEnv is the environment variable carrying the JSON-encoded Spec into a
// mover container.
const SpecEnv = "SABON_MOVER_SPEC"

// resultMarker prefixes the single machine-readable result line the mover
// prints on stdout so the orchestrator can parse it back out of the logs.
const resultMarker = "SABON_RESULT "

// Spec is the JSON contract the orchestrator passes to a mover. The restic
// repository, password and backend credentials travel separately as ordinary
// environment variables (RESTIC_REPOSITORY, RESTIC_PASSWORD, AWS_*, …) that
// restic reads directly.
type Spec struct {
	// App is the repository/app name; used as the primary restic tag and host.
	App string `json:"app"`
	// Host is the restic snapshot host (defaults to App).
	Host string `json:"host"`
	// Sources are the paths inside the mover to back up (e.g. /data/<name>).
	Sources []string `json:"sources"`
	// Excludes are restic --exclude patterns.
	Excludes []string `json:"excludes,omitempty"`
	// Tags are extra restic tags (App is always added).
	Tags []string `json:"tags,omitempty"`
	// Retention is the forget policy; when empty, forget is skipped.
	Retention api.Retention `json:"retention,omitempty"`
	// Action selects the mover operation: "backup" (default), "restore", or
	// "snapshots".
	Action string `json:"action,omitempty"`
	// SnapshotID is the snapshot to restore (restore action; default "latest").
	SnapshotID string `json:"snapshot_id,omitempty"`
	// RestoreTarget is the path restic restores into (restore action): "/" for
	// in-place (sources mounted rw), or a staging path like "/restore".
	RestoreTarget string `json:"restore_target,omitempty"`
	// Include limits a restore to matching paths (restic --include).
	Include []string `json:"include,omitempty"`
	// ExtraArgs are extra global restic flags prepended to every invocation.
	ExtraArgs []string `json:"extra_args,omitempty"`
}

// Result is the machine-readable outcome the mover emits and the orchestrator
// records as metrics.
type Result struct {
	SnapshotID     string  `json:"snapshot_id,omitempty"`
	FilesNew       int     `json:"files_new"`
	FilesChanged   int     `json:"files_changed"`
	DataAddedBytes uint64  `json:"data_added_bytes"`
	TotalBytes     uint64  `json:"total_bytes_processed"`
	BackupSeconds  float64 `json:"backup_seconds"`
}

// resticSummary is the subset of restic's `backup --json` summary message we
// care about.
type resticSummary struct {
	MessageType         string  `json:"message_type"`
	FilesNew            int     `json:"files_new"`
	FilesChanged        int     `json:"files_changed"`
	DataAdded           uint64  `json:"data_added"`
	TotalBytesProcessed uint64  `json:"total_bytes_processed"`
	TotalDuration       float64 `json:"total_duration"`
	SnapshotID          string  `json:"snapshot_id"`
}

// resticBin is the restic executable; overridable for tests.
var resticBin = "restic"

// globalArgs are extra restic flags (from Spec.ExtraArgs) prepended to every
// invocation as persistent/global flags. Set once at Execute start; each mover
// runs in its own process, so a package-level value is safe.
var globalArgs []string

// execRestic runs restic with the given args, streaming combined output to w.
// It is a variable so tests can stub restic invocation.
var execRestic = func(ctx context.Context, w io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, resticBin, args...)
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
}

// runCmd runs a restic subcommand with globalArgs prepended (persistent flags
// go before the subcommand).
func runCmd(ctx context.Context, w io.Writer, args ...string) error {
	if len(globalArgs) > 0 {
		args = append(append([]string(nil), globalArgs...), args...)
	}
	return execRestic(ctx, w, args...)
}

// Mover actions.
const (
	ActionBackup    = "backup"
	ActionRestore   = "restore"
	ActionSnapshots = "snapshots"
	ActionCheck     = "check"
	ActionPrune     = "prune"
)

// Execute is the mover-side entrypoint (`sabon mover`). It reads the Spec from
// the environment and performs the requested action against the restic repo.
func Execute(ctx context.Context, stdout, stderr io.Writer) error {
	raw := os.Getenv(SpecEnv)
	if raw == "" {
		return fmt.Errorf("%s is empty", SpecEnv)
	}
	var spec Spec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return fmt.Errorf("decode %s: %w", SpecEnv, err)
	}
	host := spec.Host
	if host == "" {
		host = spec.App
	}
	globalArgs = spec.ExtraArgs

	switch spec.Action {
	case "", ActionBackup:
		return runBackup(ctx, spec, host, stdout, stderr)
	case ActionSnapshots:
		unlockStale(ctx, stderr)
		return runCmd(ctx, stdout, "snapshots")
	case ActionCheck:
		unlockStale(ctx, stderr)
		return runCmd(ctx, stdout, "check")
	case ActionPrune:
		unlockStale(ctx, stderr)
		return runCmd(ctx, stdout, "prune")
	case ActionRestore:
		return runRestore(ctx, spec, stdout)
	default:
		return fmt.Errorf("unknown mover action %q", spec.Action)
	}
}

// runBackup initialises the repo if needed, backs up, and applies retention.
func runBackup(ctx context.Context, spec Spec, host string, stdout, stderr io.Writer) error {
	if len(spec.Sources) == 0 {
		return fmt.Errorf("mover spec has no sources")
	}
	// Ensure the repository is initialised. `restic cat config` is a cheap
	// existence probe that does not lock the repo.
	if err := runCmd(ctx, io.Discard, "cat", "config"); err != nil {
		_, _ = fmt.Fprintln(stderr, "sabon: initialising restic repository")
		if err := runCmd(ctx, stderr, "init"); err != nil {
			return fmt.Errorf("restic init: %w", err)
		}
	}

	// Remove a stale lock left by a mover that was killed mid-run.
	unlockStale(ctx, stderr)

	// Back up, parsing the --json summary as it streams.
	res, err := backup(ctx, spec, host, stdout)
	if err != nil {
		return fmt.Errorf("restic backup: %w", err)
	}
	if out, mErr := json.Marshal(res); mErr == nil {
		_, _ = fmt.Fprintln(stdout, resultMarker+string(out))
	}

	// Apply retention (mark old snapshots for removal). Scoped to this app's
	// host+tag so a shared repo never forgets another app's snapshots. Pruning
	// (repack / space reclaim) is a separate, less frequent job — the `prune` cron.
	// Never run `forget` without at least one keep flag: restic would then treat
	// every matching snapshot as expired and delete them all. ForgetArgs() drops
	// zero/negative values, so an unset or invalid policy yields no args -> skip.
	if fargs := spec.Retention.ForgetArgs(); len(fargs) > 0 {
		args := append([]string{"forget", "--host", host, "--tag", spec.App}, fargs...)
		if err := runCmd(ctx, stdout, args...); err != nil {
			return fmt.Errorf("restic forget: %w", err)
		}
	}
	return nil
}

// unlockStale clears any restic lock left by a killed mover. sabon serialises
// per repository, so no other live mover holds this repo — any lock present is
// from a dead process. --remove-all is therefore safe and, unlike a plain
// unlock, also clears a fresh lock whose foreign container hostname keeps
// restic from recognising it as stale.
func unlockStale(ctx context.Context, w io.Writer) {
	if err := runCmd(ctx, io.Discard, "unlock", "--remove-all"); err != nil {
		_, _ = fmt.Fprintln(w, "sabon: restic unlock:", err)
	}
}

// runRestore restores a snapshot into the mover's restore target.
func runRestore(ctx context.Context, spec Spec, stdout io.Writer) error {
	if spec.RestoreTarget == "" {
		return fmt.Errorf("restore: empty restore target")
	}
	unlockStale(ctx, stdout)
	snap := spec.SnapshotID
	if snap == "" {
		snap = "latest"
	}
	args := []string{"restore", snap, "--target", spec.RestoreTarget}
	for _, inc := range spec.Include {
		args = append(args, "--include", inc)
	}
	if err := runCmd(ctx, stdout, args...); err != nil {
		return fmt.Errorf("restic restore: %w", err)
	}
	return nil
}

// backup runs `restic backup --json` and extracts the summary.
func backup(ctx context.Context, spec Spec, host string, w io.Writer) (Result, error) {
	args := []string{"backup", "--json", "--host", host, "--tag", spec.App}
	for _, t := range spec.Tags {
		args = append(args, "--tag", t)
	}
	for _, e := range spec.Excludes {
		args = append(args, "--exclude", e)
	}
	args = append(args, spec.Sources...)

	pr, pw := io.Pipe()
	var summary resticSummary
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := sc.Bytes()
			var msg resticSummary
			if err := json.Unmarshal(line, &msg); err == nil && msg.MessageType == "summary" {
				summary = msg
			}
			// Forward everything for transparent logs.
			_, _ = fmt.Fprintln(w, strings.TrimRight(string(line), "\n"))
		}
	}()
	err := runCmd(ctx, pw, args...)
	_ = pw.Close()
	<-done

	res := Result{
		SnapshotID:     summary.SnapshotID,
		FilesNew:       summary.FilesNew,
		FilesChanged:   summary.FilesChanged,
		DataAddedBytes: summary.DataAdded,
		TotalBytes:     summary.TotalBytesProcessed,
		BackupSeconds:  summary.TotalDuration,
	}
	return res, err
}

// ParseResult extracts a Result from a mover's log output (the orchestrator
// side). Returns false when no result marker is present.
func ParseResult(logs string) (Result, bool) {
	for _, line := range strings.Split(logs, "\n") {
		i := strings.Index(line, resultMarker)
		if i < 0 {
			continue
		}
		var r Result
		if err := json.Unmarshal([]byte(line[i+len(resultMarker):]), &r); err == nil {
			return r, true
		}
	}
	return Result{}, false
}
