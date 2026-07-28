package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/davidborzek/sabon/api"
	"github.com/davidborzek/sabon/internal/config"
	"github.com/davidborzek/sabon/internal/discovery"
	"github.com/davidborzek/sabon/internal/snapshot"
	"github.com/docker/docker/api/types/mount"
)

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok {
			m[k] = v
		}
	}
	return m
}
func TestResticEnvRemoteExpandsRepo(t *testing.T) {
	t.Setenv("R2_ENDPOINT", "https://acct.r2.cloudflarestorage.com")
	t.Setenv("RESTIC_PASSWORD", "secret")
	o := &Orchestrator{}
	target := api.Target{Name: "offsite", Repo: "s3:${R2_ENDPOINT}/bucket/{app}"}
	env, err := o.resticEnv(target, "immich")
	if err != nil {
		t.Fatalf("resticEnv: %v", err)
	}
	m := envMap(env)
	want := "s3:https://acct.r2.cloudflarestorage.com/bucket/immich"
	if m["RESTIC_REPOSITORY"] != want {
		t.Errorf("RESTIC_REPOSITORY = %q, want %q", m["RESTIC_REPOSITORY"], want)
	}
	if m["RESTIC_PASSWORD"] != "secret" {
		t.Errorf("RESTIC_PASSWORD = %q", m["RESTIC_PASSWORD"])
	}
}

func TestResticEnvLocal(t *testing.T) {
	t.Setenv("RESTIC_PASSWORD", "secret")
	o := &Orchestrator{}
	env, err := o.resticEnv(api.Target{Name: "onsite", Path: "/mnt/backup"}, "immich")
	if err != nil {
		t.Fatalf("resticEnv: %v", err)
	}
	if envMap(env)["RESTIC_REPOSITORY"] != repoMount {
		t.Errorf("RESTIC_REPOSITORY = %q, want %q", envMap(env)["RESTIC_REPOSITORY"], repoMount)
	}
}

func TestResticEnvMissingPassword(t *testing.T) {
	o := &Orchestrator{}
	target := api.Target{Name: "onsite", Path: "/mnt/backup", PasswordEnv: "SABON_TEST_NO_SUCH_PW"}
	if _, err := o.resticEnv(target, "immich"); err == nil {
		t.Fatal("expected error when password env is empty")
	}
}

func TestMoverSpecRetentionOverride(t *testing.T) {
	o := &Orchestrator{}
	job := discovery.Job{
		App: "immich",
		Spec: api.Spec{
			Exclude: []string{"**/postgres"},
			Targets: []api.TargetSpec{{Name: "onsite", Retention: api.Retention{Daily: 1}}},
		},
		Sources: []discovery.Source{{Name: "data", Type: mount.TypeBind, Ref: "/mnt/data/immich"}},
	}
	target := api.Target{Name: "onsite", Path: "/mnt/backup", Retention: api.Retention{Daily: 99}}
	spec := o.moverSpec(job, target)
	if len(spec.Sources) != 1 || spec.Sources[0] != "/data/data" {
		t.Fatalf("Sources = %v", spec.Sources)
	}
	if spec.Retention.Daily != 1 {
		t.Errorf("Retention.Daily = %d, want 1 (label override)", spec.Retention.Daily)
	}
	if spec.Host != "immich" {
		t.Errorf("Host = %q", spec.Host)
	}
}

func TestSanitize(t *testing.T) {
	if got := sanitize("im/mich:1"); got != "im-mich-1" {
		t.Errorf("sanitize = %q", got)
	}
}

func TestResticEnvPasswordFile(t *testing.T) {
	pf := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(pf, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := &Orchestrator{cfg: &config.Config{CacheVolume: "sabon-cache"}}
	target := api.Target{Name: "onsite", Path: "/mnt/backup", PasswordFile: pf}
	env, err := o.resticEnv(target, "immich")
	if err != nil {
		t.Fatalf("resticEnv: %v", err)
	}
	m := envMap(env)
	if m["RESTIC_PASSWORD_FILE"] != passwordMount {
		t.Errorf("RESTIC_PASSWORD_FILE = %q, want %q", m["RESTIC_PASSWORD_FILE"], passwordMount)
	}
	if _, ok := m["RESTIC_PASSWORD"]; ok {
		t.Error("RESTIC_PASSWORD must not be set when PasswordFile is used")
	}
	_, binds := o.cacheAndRepo(target, "immich")
	want := pf + ":" + passwordMount + ":ro"
	found := false
	for _, b := range binds {
		if b == want {
			found = true
		}
	}
	if !found {
		t.Errorf("password-file bind %q missing from %v", want, binds)
	}
}

func TestResticEnvPasswordFileMissing(t *testing.T) {
	o := &Orchestrator{}
	target := api.Target{Name: "onsite", Path: "/mnt/backup", PasswordFile: "/no/such/sabon/pw"}
	if _, err := o.resticEnv(target, "immich"); err == nil {
		t.Fatal("expected error for a missing password file")
	}
}

func TestResticEnvCredentialsFile(t *testing.T) {
	cf := filepath.Join(t.TempDir(), "aws")
	if err := os.WriteFile(cf, []byte("[default]\naws_access_key_id=x\naws_secret_access_key=y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("R_PW_CF", "pw")
	o := &Orchestrator{cfg: &config.Config{CacheVolume: "sabon-cache"}}
	target := api.Target{Name: "offsite", Repo: "s3:http://minio/b/{app}", PasswordEnv: "R_PW_CF", CredentialsFile: cf}
	env, err := o.resticEnv(target, "immich")
	if err != nil {
		t.Fatalf("resticEnv: %v", err)
	}
	if envMap(env)["AWS_SHARED_CREDENTIALS_FILE"] != awsCredentialsMount {
		t.Errorf("AWS_SHARED_CREDENTIALS_FILE = %q, want %q", envMap(env)["AWS_SHARED_CREDENTIALS_FILE"], awsCredentialsMount)
	}
	_, binds := o.cacheAndRepo(target, "immich")
	want := cf + ":" + awsCredentialsMount + ":ro"
	found := false
	for _, b := range binds {
		if b == want {
			found = true
		}
	}
	if !found {
		t.Errorf("credentials-file bind %q missing from %v", want, binds)
	}
}

// fakeSnap is a snapshot.Snapshotter over an in-memory "on this fs" set.
type fakeSnap struct {
	mode   string
	onFS   map[string]bool
	resErr bool
}

func (f *fakeSnap) Mode() string { return f.mode }

func (f *fakeSnap) Resolve(_ context.Context, sources []snapshot.Source) ([]snapshot.Resolution, error) {
	if f.resErr {
		return nil, errors.New("unavailable")
	}
	res := make([]snapshot.Resolution, 0, len(sources))
	for _, s := range sources {
		r := snapshot.Resolution{Name: s.Name, HostPath: s.HostPath, Detail: "not on " + f.mode}
		if f.onFS[s.Name] {
			r.Snapshottable, r.Detail = true, "dataset "+s.Name
		}
		res = append(res, r)
	}
	return res, nil
}

func (f *fakeSnap) Snapshot(_ context.Context, _ string, sources []snapshot.Source) ([]snapshot.Mount, func(context.Context), error) {
	for _, s := range sources {
		if !f.onFS[s.Name] {
			return nil, nil, fmt.Errorf("source %q not on %s", s.Name, f.mode)
		}
	}
	ms := make([]snapshot.Mount, 0, len(sources))
	for _, s := range sources {
		ms = append(ms, snapshot.Mount{Name: s.Name, HostPath: "/snap/" + s.Name})
	}
	return ms, func(context.Context) {}, nil
}

func (f *fakeSnap) Reap(_ context.Context, _ []string) (int, error) { return 0, nil }

func testOrch(mode string, snaps ...snapshot.Snapshotter) *Orchestrator {
	return &Orchestrator{
		cfg:   &config.Config{Snapshot: mode},
		host:  fakeHost{},
		snaps: snaps,
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func binds(names ...string) []discovery.Source {
	s := make([]discovery.Source, 0, len(names))
	for _, n := range names {
		s = append(s, discovery.Source{Name: n, Type: mount.TypeBind, Ref: "/host/" + n})
	}
	return s
}

func mountsByTarget(ms []mount.Mount) map[string]mount.Mount {
	m := map[string]mount.Mount{}
	for _, x := range ms {
		m[x.Target] = x
	}
	return m
}

func TestSourceMountsForAutoMixed(t *testing.T) {
	fake := &fakeSnap{mode: "fakefs", onFS: map[string]bool{"onfs": true}}
	job := discovery.Job{App: "x", Sources: binds("onfs", "offfs")}
	ms, _, err := testOrch("auto", fake).sourceMountsFor(context.Background(), job)
	if err != nil {
		t.Fatalf("sourceMountsFor(auto): %v", err)
	}
	if len(ms) != 2 {
		t.Fatalf("want 2 mounts, got %d: %+v", len(ms), ms)
	}
	got := mountsByTarget(ms)
	if got["/data/onfs"].Source != "/snap/onfs" {
		t.Errorf("onfs must be snapshotted, got %+v", got["/data/onfs"])
	}
	if got["/data/offfs"].Source != "/host/offfs" {
		t.Errorf("offfs must be mounted live, got %+v", got["/data/offfs"])
	}
}

func TestSourceMountsForStrictFailsOffFS(t *testing.T) {
	fake := &fakeSnap{mode: "fakefs", onFS: map[string]bool{"onfs": true}}
	job := discovery.Job{App: "x", Sources: binds("onfs", "offfs")}
	if _, _, err := testOrch("fakefs", fake).sourceMountsFor(context.Background(), job); err == nil {
		t.Error("strict mode must fail when a source is not on the provider fs")
	}
}

func TestSourceMountsForAutoProviderUnavailableAllLive(t *testing.T) {
	fake := &fakeSnap{mode: "fakefs", resErr: true}
	job := discovery.Job{App: "x", Sources: binds("a", "b")}
	ms, _, err := testOrch("auto", fake).sourceMountsFor(context.Background(), job)
	if err != nil {
		t.Fatalf("auto must not fail when a provider is unavailable: %v", err)
	}
	got := mountsByTarget(ms)
	if len(ms) != 2 || got["/data/a"].Source != "/host/a" || got["/data/b"].Source != "/host/b" {
		t.Errorf("all sources must be live, got %+v", ms)
	}
}

func TestSourceMountsForUnknownMode(t *testing.T) {
	job := discovery.Job{App: "x", Sources: binds("a")}
	if _, _, err := testOrch("btrfs", &fakeSnap{mode: "fakefs"}).sourceMountsFor(context.Background(), job); err == nil {
		t.Error("unknown snapshot mode must error")
	}
}

// fakeHost is a no-op engine.Host for tests; foreign/paths configure
// VolumeHostPath for the snapshot-resolution tests.
type fakeHost struct {
	foreign map[string]string // volume name -> non-local reason
	paths   map[string]string // volume name -> host mountpoint
}

func (fakeHost) EnsureCache(context.Context, string) error { return nil }
func (h fakeHost) VolumeHostPath(_ context.Context, name string) (string, string, error) {
	return h.paths[name], h.foreign[name], nil
}
func (fakeHost) RunningMoverBinds(context.Context) ([]string, error) { return nil, nil }

func TestSourceMountsForAutoForeignVolumeGoesLive(t *testing.T) {
	// A foreign-backed volume (local driver + nfs options) must be mounted live
	// by name in auto — never snapshotted — even though the provider would claim
	// its resolved path.
	fake := &fakeSnap{mode: "fakefs", onFS: map[string]bool{"nfsvol": true}}
	o := testOrch("auto", fake)
	o.host = fakeHost{
		paths:   map[string]string{"nfsvol": "/var/lib/docker/volumes/nfsvol/_data"},
		foreign: map[string]string{"nfsvol": "nfs mount, not local storage"},
	}
	job := discovery.Job{App: "x", Sources: []discovery.Source{{Name: "nfsvol", Type: mount.TypeVolume, Ref: "nfsvol"}}}
	ms, _, err := o.sourceMountsFor(context.Background(), job)
	if err != nil {
		t.Fatalf("sourceMountsFor(auto): %v", err)
	}
	if len(ms) != 1 {
		t.Fatalf("want 1 mount, got %d: %+v", len(ms), ms)
	}
	if ms[0].Type != mount.TypeVolume || ms[0].Source != "nfsvol" {
		t.Errorf("foreign volume must be mounted live by name, got %+v", ms[0])
	}
}

func TestColdStopRefcount(t *testing.T) {
	var cs coldStop
	stops, starts := 0, 0
	stop := func() error { stops++; return nil }
	start := func() { starts++ }

	if err := cs.hold(stop); err != nil {
		t.Fatal(err)
	}
	if err := cs.hold(stop); err != nil { // second holder must not stop again
		t.Fatal(err)
	}
	if stops != 1 {
		t.Fatalf("stops = %d, want 1", stops)
	}
	cs.release(start)
	if starts != 0 {
		t.Fatalf("started while a holder remains: starts = %d", starts)
	}
	cs.release(start) // last holder starts
	if starts != 1 {
		t.Fatalf("starts = %d, want 1", starts)
	}
	// a fresh cold backup after everyone left stops again
	if err := cs.hold(stop); err != nil {
		t.Fatal(err)
	}
	if stops != 2 {
		t.Fatalf("re-hold should stop again: stops = %d", stops)
	}
	cs.release(start)
}

func TestColdStopHoldStopError(t *testing.T) {
	var cs coldStop
	if err := cs.hold(func() error { return errors.New("boom") }); err == nil {
		t.Fatal("hold must surface the stop error")
	}
	// a failed stop is not counted: the next holder retries and succeeds
	stopped := false
	if err := cs.hold(func() error { stopped = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !stopped {
		t.Fatal("a failed stop must not be counted; next hold should retry it")
	}
}

func TestColdStopConcurrent(t *testing.T) {
	var cs coldStop
	var stops, starts atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = cs.hold(func() error { stops.Add(1); return nil })
			time.Sleep(time.Millisecond)
			cs.release(func() { starts.Add(1) })
		}()
	}
	wg.Wait()
	if got := stops.Load(); got != starts.Load() {
		t.Fatalf("unbalanced stop/start: %d stops, %d starts", got, starts.Load())
	}
	if cs.refs != 0 {
		t.Fatalf("refs = %d, want 0 after all released", cs.refs)
	}
}

func TestSourcesForTarget(t *testing.T) {
	o := &Orchestrator{}
	job := discovery.Job{
		App: "immich",
		Spec: api.Spec{
			Targets: []api.TargetSpec{
				{Name: "onsite"},
				{Name: "offsite", ExcludeVolumes: []string{"media"}, ExcludePaths: []string{"/big"}},
			},
		},
		Sources: []discovery.Source{
			{Name: "media", Type: mount.TypeVolume, Ref: "media"},
			{Name: "config", Type: mount.TypeVolume, Ref: "config"},
			{Name: "big", Type: mount.TypeBind, Ref: "/big"},
		},
	}
	if got := o.sourcesForTarget(job, api.Target{Name: "onsite"}); len(got) != 3 {
		t.Errorf("onsite (no exclusions) = %d sources, want 3", len(got))
	}
	got := o.sourcesForTarget(job, api.Target{Name: "offsite"})
	if len(got) != 1 || got[0].Name != "config" {
		t.Errorf("offsite = %+v, want just [config]", got)
	}
	if got := o.sourcesForTarget(job, api.Target{Name: "other"}); len(got) != 3 {
		t.Errorf("unlisted target = %d sources, want 3 (no exclusions)", len(got))
	}
}

func TestExpandEnvResolution(t *testing.T) {
	t.Setenv("SABON_HOOK_ENV_FALLBACK", "from-sabon")
	t.Setenv("RESTIC_PASSWORD", "toplevel-secret")
	containerEnv := map[string]string{"DB_PASSWORD": "from-container"}
	got := map[string]string{}
	for _, kv := range expandEnv(map[string]string{
		"A": "${DB_PASSWORD}",     // labelled container's own env -> wins
		"B": "${FALLBACK}",        // SABON_HOOK_ENV_FALLBACK allowlist fallback
		"C": "${RESTIC_PASSWORD}", // sabon's own secret -> unreachable, empty
	}, containerEnv) {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	if got["A"] != "from-container" {
		t.Errorf("A = %q, want from-container (labelled container env)", got["A"])
	}
	if got["B"] != "from-sabon" {
		t.Errorf("B = %q, want from-sabon (SABON_HOOK_ENV_ fallback)", got["B"])
	}
	if got["C"] != "" {
		t.Errorf("RESTIC_PASSWORD leaked into hook env: %q", got["C"])
	}
}

// With no registered providers (e.g. the swarm runtime), a strict snapshot mode
// must fail both a real run and `sabon validate`, not silently report available.
func TestSnapshotModeUnavailableWithoutProviders(t *testing.T) {
	job := discovery.Job{App: "x", Sources: binds("a")}
	o := testOrch("zfs") // no snapshot providers
	if _, _, err := o.sourceMountsFor(context.Background(), job); err == nil {
		t.Error("strict snapshot mode with no providers must fail the run")
	}
	if _, err := o.PreviewSnapshots(context.Background(), job); err == nil {
		t.Error("validate must report a strict snapshot mode unavailable when no providers are registered")
	}
}
