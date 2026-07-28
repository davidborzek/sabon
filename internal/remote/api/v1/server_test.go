package v1

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/davidborzek/sabon/api"
	"github.com/davidborzek/sabon/internal/backup"
	"github.com/davidborzek/sabon/internal/discovery"
	"github.com/davidborzek/sabon/internal/engine"
)

type fakeBackend struct {
	backups atomic.Int32
	runs    []engine.RunInfo
}

func (f *fakeBackend) ConfigTargets() []api.Target {
	return []api.Target{{Name: "onsite", Path: "/mnt/backup"}}
}
func (f *fakeBackend) Jobs(context.Context) ([]discovery.Job, error) {
	return []discovery.Job{{App: "demo", Container: "demo"}}, nil
}
func (f *fakeBackend) Snapshots(_ context.Context, _, _ string, out io.Writer) error {
	_, _ = io.WriteString(out, "snap-output")
	return nil
}
func (f *fakeBackend) Backup(context.Context, string, string) error { f.backups.Add(1); return nil }
func (f *fakeBackend) Check(context.Context, string, string) error  { return nil }
func (f *fakeBackend) Prune(context.Context, string, string) error  { return nil }
func (f *fakeBackend) Restore(context.Context, string, string, backup.RestoreOptions, io.Writer) error {
	return nil
}
func (f *fakeBackend) ListRuns(context.Context, string, string) ([]engine.RunInfo, error) {
	return f.runs, nil
}
func (f *fakeBackend) GetRun(_ context.Context, id string) (engine.RunInfo, bool, error) {
	for _, r := range f.runs {
		if r.ID == id {
			return r, true, nil
		}
	}
	return engine.RunInfo{}, false, nil
}
func (f *fakeBackend) RunLogs(_ context.Context, id string, out io.Writer) error {
	_, _ = io.WriteString(out, "log-"+id)
	return nil
}

func testServer() (*Server, *fakeBackend) {
	f := &fakeBackend{runs: []engine.RunInfo{{ID: "abc", App: "demo", Target: "onsite", Action: "backup", Status: "succeeded"}}}
	return New(f, "secret", slog.New(slog.NewTextHandler(io.Discard, nil))), f
}

func do(s *Server, method, path, token, body string) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestAuth(t *testing.T) {
	s, _ := testServer()
	for _, tc := range []struct {
		token string
		want  int
	}{{"", 401}, {"wrong", 401}, {"secret", 200}} {
		if rr := do(s, "GET", "/api/v1/config", tc.token, ""); rr.Code != tc.want {
			t.Errorf("token %q: got %d, want %d", tc.token, rr.Code, tc.want)
		}
	}
}

func TestGetRun(t *testing.T) {
	s, _ := testServer()
	if rr := do(s, "GET", "/api/v1/runs/abc", "secret", ""); rr.Code != 200 {
		t.Errorf("existing run: %d", rr.Code)
	}
	if rr := do(s, "GET", "/api/v1/runs/nope", "secret", ""); rr.Code != 404 {
		t.Errorf("missing run: %d", rr.Code)
	}
}

func TestGetRunLogs(t *testing.T) {
	s, _ := testServer()
	rr := do(s, "GET", "/api/v1/runs/abc/logs", "secret", "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "log-abc") {
		t.Errorf("logs: code=%d body=%q", rr.Code, rr.Body.String())
	}
	if rr := do(s, "GET", "/api/v1/runs/nope/logs", "secret", ""); rr.Code != 404 {
		t.Errorf("missing run logs: %d", rr.Code)
	}
}

func TestPostBackupAcceptedAndInvokes(t *testing.T) {
	s, f := testServer()
	rr := do(s, "POST", "/api/v1/apps/demo/backup", "secret", "")
	if rr.Code != 202 {
		t.Fatalf("backup: got %d, want 202", rr.Code)
	}
	deadline := time.Now().Add(2 * time.Second)
	for f.backups.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := f.backups.Load(); got != 1 {
		t.Errorf("backend.Backup invoked %d times, want 1", got)
	}
}

func TestPostRestoreRequiresTarget(t *testing.T) {
	s, _ := testServer()
	if rr := do(s, "POST", "/api/v1/apps/demo/restore", "secret", "{}"); rr.Code != 400 {
		t.Errorf("restore without target: got %d, want 400", rr.Code)
	}
	if rr := do(s, "POST", "/api/v1/apps/demo/restore", "secret", `{"target":"onsite"}`); rr.Code != 202 {
		t.Errorf("restore with target: got %d, want 202", rr.Code)
	}
}

func TestMethodRouting(t *testing.T) {
	s, _ := testServer()
	// GET on a POST-only route must not trigger a backup; ServeMux returns 405.
	if rr := do(s, "GET", "/api/v1/apps/demo/backup", "secret", ""); rr.Code != 405 {
		t.Errorf("GET on backup route: got %d, want 405", rr.Code)
	}
}

func TestNoTokenAllowsUnauthenticated(t *testing.T) {
	s := New(&fakeBackend{}, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if rr := do(s, "GET", "/api/v1/config", "", ""); rr.Code != 200 {
		t.Errorf("empty token must allow unauthenticated requests: got %d", rr.Code)
	}
}
