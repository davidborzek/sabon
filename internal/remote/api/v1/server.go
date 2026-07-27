// Package v1 serves an optional, token-authenticated HTTP control API that
// exposes sabon's operations — show config, list jobs, list snapshots, trigger
// backup/check/prune/restore, and browse run history.
//
// It is disabled unless SABON_API_ADDR is set, and every request must carry the
// SABON_API_TOKEN as a bearer token. Mutating operations are fire-and-forget:
// the POST returns 202 and the run shows up under GET /runs once its mover
// exists. Run history is durable — it is derived from the retained mover
// containers (see SABON_MOVER_HISTORY), so it survives a sabon restart.
package v1

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/davidborzek/sabon/api"
	"github.com/davidborzek/sabon/internal/backup"
	"github.com/davidborzek/sabon/internal/discovery"
)

// Backend is the set of operations the API exposes. *reconcile.Reconciler
// implements it, so an API-triggered run shares target resolution, metrics and
// notifications with a scheduled one. Backup/Check/Prune take an empty target to
// mean "all of the app's targets". Run history (ListRuns/GetRun/RunLogs) is read
// back from the retained mover containers.
type Backend interface {
	ConfigTargets() []api.Target
	Jobs(ctx context.Context) ([]discovery.Job, error)
	Snapshots(ctx context.Context, app, target string, out io.Writer) error
	Backup(ctx context.Context, app, target string) error
	Check(ctx context.Context, app, target string) error
	Prune(ctx context.Context, app, target string) error
	Restore(ctx context.Context, app, target string, opts backup.RestoreOptions, out io.Writer) error
	ListRuns(ctx context.Context, app, target string) ([]backup.RunInfo, error)
	GetRun(ctx context.Context, id string) (backup.RunInfo, bool, error)
	RunLogs(ctx context.Context, id string, out io.Writer) error
}

// Server is the HTTP control API.
type Server struct {
	backend Backend
	token   string
	log     *slog.Logger
	ctx     context.Context // long-lived; fire-and-forget ops use it, not the request ctx
}

// New returns a Server backed by b and authenticated with token. Until Serve is
// called, triggered operations use a background context.
func New(b Backend, token string, log *slog.Logger) *Server {
	return &Server{backend: b, token: token, log: log, ctx: context.Background()}
}

// Handler returns the authenticated router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/config", s.getConfig)
	mux.HandleFunc("GET /api/v1/jobs", s.getJobs)
	mux.HandleFunc("GET /api/v1/apps/{app}/snapshots", s.getSnapshots)
	mux.HandleFunc("POST /api/v1/apps/{app}/backup", s.postBackup)
	mux.HandleFunc("POST /api/v1/apps/{app}/check", s.postCheck)
	mux.HandleFunc("POST /api/v1/apps/{app}/prune", s.postPrune)
	mux.HandleFunc("POST /api/v1/apps/{app}/restore", s.postRestore)
	mux.HandleFunc("GET /api/v1/runs", s.getRuns)
	mux.HandleFunc("GET /api/v1/runs/{id}", s.getRun)
	mux.HandleFunc("GET /api/v1/runs/{id}/logs", s.getRunLogs)
	return s.authenticate(mux)
}

// Serve starts the API on addr and shuts it down when ctx is cancelled. ctx also
// drives fire-and-forget ops, so a backup outlives the request that triggered
// it. A blank addr is a no-op.
func (s *Server) Serve(ctx context.Context, addr string) {
	if addr == "" {
		return
	}
	if s.token == "" {
		s.log.Warn("api server has NO token: requests are unauthenticated and can trigger backups and restores; set SABON_API_TOKEN, or bind SABON_API_ADDR to loopback behind a trusted proxy", "addr", addr)
	}
	s.ctx = ctx
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	go func() {
		s.log.Info("api server listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.log.Error("api server error", "error", err)
		}
	}()
}

// authenticate enforces the bearer token on every request in constant time. An
// empty token disables authentication entirely (see the warning in Serve).
func (s *Server) authenticate(next http.Handler) http.Handler {
	want := []byte(s.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(want) > 0 {
			tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || subtle.ConstantTimeCompare([]byte(tok), want) != 1 {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
