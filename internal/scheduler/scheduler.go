// Package scheduler runs backup jobs on cron schedules and keeps the active set
// in sync with what discovery finds.
package scheduler

import (
	"log/slog"
	"sync"

	"github.com/robfig/cron/v3"
)

// Job is one scheduled backup (an app to a target).
type Job struct {
	Key      string // stable identity, e.g. "<app>|<target>"
	Schedule string // 6-field cron (seconds first)
	// Fingerprint captures the resolved job (sources + spec-derived fields) so a
	// change re-registers even when the cron string is unchanged.
	Fingerprint string
	Run         func()
}

// Scheduler owns a cron and reconciles its entries against a desired set.
type Scheduler struct {
	cron *cron.Cron
	log  *slog.Logger

	mu      sync.Mutex
	entries map[string]entry
}

type entry struct {
	id          cron.EntryID
	schedule    string
	fingerprint string
}

// New returns a Scheduler using a seconds-aware cron parser.
func New(log *slog.Logger) *Scheduler {
	return &Scheduler{
		cron:    cron.New(cron.WithSeconds()),
		log:     log,
		entries: map[string]entry{},
	}
}

// Start begins firing schedules.
func (s *Scheduler) Start() { s.cron.Start() }

// Stop halts the cron and waits for running jobs to finish.
func (s *Scheduler) Stop() { <-s.cron.Stop().Done() }

// Sync makes the active schedule set exactly match desired: it adds new jobs,
// re-registers ones whose schedule changed, and removes stale ones. Invalid
// cron expressions are logged and skipped.
func (s *Scheduler) Sync(desired []Job) {
	s.mu.Lock()
	defer s.mu.Unlock()

	want := make(map[string]Job, len(desired))
	for _, j := range desired {
		want[j.Key] = j
	}

	// Remove or update existing entries.
	for key, e := range s.entries {
		j, keep := want[key]
		if !keep || j.Schedule != e.schedule || j.Fingerprint != e.fingerprint {
			s.cron.Remove(e.id)
			delete(s.entries, key)
		}
	}

	// Add new or changed entries.
	for key, j := range want {
		if _, ok := s.entries[key]; ok {
			continue
		}
		id, err := s.cron.AddFunc(j.Schedule, j.Run)
		if err != nil {
			s.log.Warn("invalid schedule, skipping", "key", key, "schedule", j.Schedule, "error", err)
			continue
		}
		s.entries[key] = entry{id: id, schedule: j.Schedule, fingerprint: j.Fingerprint}
	}
}

// Len returns the number of active scheduled jobs.
func (s *Scheduler) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}
