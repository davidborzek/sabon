// Package metrics exposes sabon's Prometheus metrics and health endpoints on a
// private registry.
package metrics

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds sabon's instruments on a dedicated registry.
type Metrics struct {
	reg        *prometheus.Registry
	readyState atomic.Bool // real readiness for /readyz (true after a successful reconcile)
	mu         sync.Mutex
	seen       map[string]bool // apps with live series, for cardinality pruning

	buildInfo   *prometheus.GaugeVec
	ready       prometheus.Gauge
	managedJobs prometheus.Gauge

	reconciles     *prometheus.CounterVec
	lastReconcile  prometheus.Gauge
	watchRestarts  prometheus.Counter
	deprecatedLbls *prometheus.CounterVec

	backups        *prometheus.CounterVec
	backupDuration *prometheus.HistogramVec
	lastSuccess    *prometheus.GaugeVec
	filesNew       *prometheus.GaugeVec
	dataAdded      *prometheus.GaugeVec
	checks         *prometheus.CounterVec
	lastCheck      *prometheus.GaugeVec
	prunes         *prometheus.CounterVec
	lastPrune      *prometheus.GaugeVec
}

// New builds the registry and registers every metric.
func New(version string) *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	m := &Metrics{
		reg: reg,
		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sabon_build_info", Help: "Build information; constant 1.",
		}, []string{"version"}),
		ready: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sabon_ready", Help: "1 when the last reconcile succeeded.",
		}),
		managedJobs: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sabon_managed_jobs", Help: "Number of discovered backup jobs.",
		}),
		reconciles: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sabon_reconciles_total", Help: "Reconcile runs by result.",
		}, []string{"result"}),
		lastReconcile: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "sabon_last_reconcile_timestamp_seconds", Help: "Unix time of the last reconcile.",
		}),
		watchRestarts: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sabon_watch_restarts_total", Help: "Docker event stream resubscriptions.",
		}),
		deprecatedLbls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sabon_deprecated_label_total", Help: "Uses of a deprecated label.",
		}, []string{"label"}),
		backups: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sabon_backups_total", Help: "Backup runs by app, target and result.",
		}, []string{"app", "target", "result"}),
		backupDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "sabon_backup_duration_seconds", Help: "Backup run duration.",
			Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600},
		}, []string{"app", "target"}),
		lastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sabon_last_backup_success_timestamp_seconds", Help: "Unix time of the last successful backup.",
		}, []string{"app", "target"}),
		filesNew: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sabon_backup_files_new", Help: "New files in the most recent backup.",
		}, []string{"app", "target"}),
		dataAdded: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sabon_backup_data_added_bytes", Help: "Bytes added by the most recent backup.",
		}, []string{"app", "target"}),
		checks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sabon_checks_total", Help: "Repository check runs by app, target and result.",
		}, []string{"app", "target", "result"}),
		lastCheck: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sabon_last_check_success_timestamp_seconds", Help: "Unix time of the last successful repository check.",
		}, []string{"app", "target"}),
		prunes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sabon_prunes_total", Help: "Repository prune runs by app, target and result.",
		}, []string{"app", "target", "result"}),
		lastPrune: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sabon_last_prune_success_timestamp_seconds", Help: "Unix time of the last successful repository prune.",
		}, []string{"app", "target"}),
	}

	reg.MustRegister(
		m.buildInfo, m.ready, m.managedJobs, m.reconciles, m.lastReconcile,
		m.watchRestarts, m.deprecatedLbls, m.backups, m.backupDuration,
		m.lastSuccess, m.filesNew, m.dataAdded, m.checks, m.lastCheck, m.prunes, m.lastPrune,
	)
	m.buildInfo.WithLabelValues(version).Set(1)
	return m
}

// ObserveReconcile records a reconcile outcome.
func (m *Metrics) ObserveReconcile(ok bool) {
	m.reconciles.WithLabelValues(result(ok)).Inc()
	m.lastReconcile.SetToCurrentTime()
	m.ready.Set(b2f(ok))
	if ok {
		m.readyState.Store(true)
	}
}

// SetManagedJobs records how many jobs are currently managed.
func (m *Metrics) SetManagedJobs(n int) { m.managedJobs.Set(float64(n)) }

// ObserveWatchRestart records a Docker event stream resubscription.
func (m *Metrics) ObserveWatchRestart() { m.watchRestarts.Inc() }

// ObserveDeprecatedLabel records a use of a deprecated label.
func (m *Metrics) ObserveDeprecatedLabel(label string) { m.deprecatedLbls.WithLabelValues(label).Inc() }

// ObserveBackup records a backup run and its result stats.
func (m *Metrics) ObserveBackup(app, target string, ok bool, dur time.Duration, filesNew int, dataAdded uint64) {
	m.markSeen(app)
	m.backups.WithLabelValues(app, target, result(ok)).Inc()
	m.backupDuration.WithLabelValues(app, target).Observe(dur.Seconds())
	if ok {
		m.lastSuccess.WithLabelValues(app, target).SetToCurrentTime()
		m.filesNew.WithLabelValues(app, target).Set(float64(filesNew))
		m.dataAdded.WithLabelValues(app, target).Set(float64(dataAdded))
	}
}

// ObserveCheck records a repository check run.
func (m *Metrics) ObserveCheck(app, target string, ok bool) {
	m.markSeen(app)
	m.checks.WithLabelValues(app, target, result(ok)).Inc()
	if ok {
		m.lastCheck.WithLabelValues(app, target).SetToCurrentTime()
	}
}

// ObservePrune records a repository prune run.
func (m *Metrics) ObservePrune(app, target string, ok bool) {
	m.markSeen(app)
	m.prunes.WithLabelValues(app, target, result(ok)).Inc()
	if ok {
		m.lastPrune.WithLabelValues(app, target).SetToCurrentTime()
	}
}

// Serve exposes /metrics, /healthz and /readyz until ctx is cancelled. A blank
// addr disables the server.
func (m *Metrics) Serve(ctx context.Context, addr string, log *slog.Logger) {
	if addr == "" {
		return
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !m.readyState.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("metrics server error", "error", err)
		}
	}()
}

func result(ok bool) string {
	if ok {
		return "success"
	}
	return "error"
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// markSeen records an app so ForgetAbsent can later drop its series.
func (m *Metrics) markSeen(app string) {
	m.mu.Lock()
	if m.seen == nil {
		m.seen = map[string]bool{}
	}
	m.seen[app] = true
	m.mu.Unlock()
}

// ForgetAbsent drops metric series for apps no longer discovered, bounding
// label cardinality as apps come and go.
func (m *Metrics) ForgetAbsent(active map[string]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for app := range m.seen {
		if active[app] {
			continue
		}
		lbl := prometheus.Labels{"app": app}
		for _, v := range []interface {
			DeletePartialMatch(prometheus.Labels) int
		}{
			m.backups, m.backupDuration, m.lastSuccess, m.filesNew, m.dataAdded,
			m.checks, m.lastCheck, m.prunes, m.lastPrune,
		} {
			v.DeletePartialMatch(lbl)
		}
		delete(m.seen, app)
	}
}
