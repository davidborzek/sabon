package v1

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/davidborzek/sabon/api"
	"github.com/davidborzek/sabon/internal/backup"
	"github.com/davidborzek/sabon/internal/discovery"
)

// ── reads ────────────────────────────────────────────────────────────────────

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"targets": targetViews(s.backend.ConfigTargets())})
}

func (s *Server) getJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.backend.Jobs(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobViews(jobs)})
}

func (s *Server) getSnapshots(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	target := r.URL.Query().Get("target")
	var buf bytes.Buffer
	if err := s.backend.Snapshots(r.Context(), app, target, &buf); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"app": app, "target": target, "output": buf.String()})
}

// ── run history (durable, read from the retained mover containers) ───────────

func (s *Server) getRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.backend.ListRuns(r.Context(), r.URL.Query().Get("app"), r.URL.Query().Get("target"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	run, ok, err := s.backend.GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) getRunLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, ok, err := s.backend.GetRun(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "run not found")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_ = s.backend.RunLogs(r.Context(), id, w)
}

// ── writes: fire-and-forget. POST returns 202; the run then appears under /runs
// as "running" once its mover container is created, then succeeded/failed. ─────

func (s *Server) postBackup(w http.ResponseWriter, r *http.Request) {
	app, target := r.PathValue("app"), r.URL.Query().Get("target")
	s.trigger(w, "backup", app, target, func(ctx context.Context) error {
		return s.backend.Backup(ctx, app, target)
	})
}

func (s *Server) postCheck(w http.ResponseWriter, r *http.Request) {
	app, target := r.PathValue("app"), r.URL.Query().Get("target")
	s.trigger(w, "check", app, target, func(ctx context.Context) error {
		return s.backend.Check(ctx, app, target)
	})
}

func (s *Server) postPrune(w http.ResponseWriter, r *http.Request) {
	app, target := r.PathValue("app"), r.URL.Query().Get("target")
	s.trigger(w, "prune", app, target, func(ctx context.Context) error {
		return s.backend.Prune(ctx, app, target)
	})
}

// restoreRequest is the JSON body for POST .../restore.
type restoreRequest struct {
	Target   string   `json:"target"`   // required: which target/repo to restore from
	Snapshot string   `json:"snapshot"` // default "latest"
	Into     string   `json:"into"`     // host staging dir; empty => in-place
	Stop     bool     `json:"stop"`     // stop the app during an in-place restore
	Include  []string `json:"include"`  // limit restore to matching paths
}

func (s *Server) postRestore(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	var body restoreRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Target == "" {
		writeError(w, http.StatusBadRequest, "target is required")
		return
	}
	opts := backup.RestoreOptions{Snapshot: body.Snapshot, Into: body.Into, Stop: body.Stop, Include: body.Include}
	s.trigger(w, "restore", app, body.Target, func(ctx context.Context) error {
		// restic output is captured in the mover's logs (GET /runs/{id}/logs).
		return s.backend.Restore(ctx, app, body.Target, opts, io.Discard)
	})
}

// trigger runs fn asynchronously on the server's long-lived context (so the op
// outlives the request) and returns 202. The resulting mover is observable under
// GET /runs.
func (s *Server) trigger(w http.ResponseWriter, op, app, target string, fn func(context.Context) error) {
	go func() {
		if err := fn(s.ctx); err != nil {
			s.log.Warn("api "+op+" failed", "app", app, "target", target, "error", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"op": op, "app": app, "target": target, "status": "accepted"})
}

// ── view DTOs (safe projections; never expose passwords or env) ──────────────

type targetView struct {
	Name      string        `json:"name"`
	Backend   string        `json:"backend"` // "local" | "remote"
	Path      string        `json:"path,omitempty"`
	Repo      string        `json:"repo,omitempty"`
	Schedule  string        `json:"schedule,omitempty"`
	Check     string        `json:"check,omitempty"`
	Prune     string        `json:"prune,omitempty"`
	Retention api.Retention `json:"retention"`
}

func targetViews(ts []api.Target) []targetView {
	out := make([]targetView, 0, len(ts))
	for _, t := range ts {
		out = append(out, targetView{
			Name:      t.Name,
			Backend:   string(t.Backend()),
			Path:      t.Path,
			Repo:      t.Repo,
			Schedule:  t.Schedule,
			Check:     t.Check,
			Prune:     t.Prune,
			Retention: t.Retention,
		})
	}
	return out
}

type sourceView struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Ref  string `json:"ref"`
}

type jobView struct {
	App       string       `json:"app"`
	Container string       `json:"container"`
	Snapshot  string       `json:"snapshot,omitempty"`
	Targets   []string     `json:"targets,omitempty"` // empty = all configured targets
	Tags      []string     `json:"tags,omitempty"`
	Stop      bool         `json:"stop,omitempty"`
	Sources   []sourceView `json:"sources"`
}

func jobViews(jobs []discovery.Job) []jobView {
	out := make([]jobView, 0, len(jobs))
	for _, j := range jobs {
		srcs := make([]sourceView, 0, len(j.Sources))
		for _, src := range j.Sources {
			srcs = append(srcs, sourceView{Name: src.Name, Type: string(src.Type), Ref: src.Ref})
		}
		out = append(out, jobView{
			App:       j.App,
			Container: j.Container,
			Snapshot:  j.Spec.Snapshot,
			Targets:   j.Spec.TargetNames(),
			Tags:      j.Spec.Tags,
			Stop:      j.Spec.Stop,
			Sources:   srcs,
		})
	}
	return out
}
