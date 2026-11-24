// Package schedule runs named cron jobs for a plugin runtime.
package schedule

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/robfig/cron/v3"
)

// Scheduler owns the cron jobs of one plugin runtime. Its jobs map is only
// touched by the goroutine that creates and stops the runtime; the job
// callbacks are invoked by robfig/cron goroutines and never read the map.
type Scheduler struct {
	logger *slog.Logger
	cron   *cron.Cron
	jobs   map[string]*job
}

type job struct {
	id   cron.EntryID
	busy atomic.Bool
}

// New creates a started scheduler. A nil logger uses slog.Default.
func New(logger *slog.Logger) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Scheduler{
		logger: logger,
		cron:   cron.New(cron.WithLogger(cron.DiscardLogger)),
		jobs:   make(map[string]*job),
	}
	s.cron.Start()
	return s
}

// Validate reports whether expression is a supported schedule.
func (s *Scheduler) Validate(expr string) error {
	if _, err := parseSchedule(expr); err != nil {
		return fmt.Errorf("invalid schedule %q: %w", expr, err)
	}
	return nil
}

// parseSchedule parses an expression in the host's local time zone. Timezone
// prefixes are rejected: they would override the documented behavior, and
// robfig/cron v3.0.1 panics when a prefix is not followed by a space.
func parseSchedule(expr string) (cron.Schedule, error) {
	trimmed := strings.TrimSpace(expr)
	if trimmed == "" {
		return nil, errors.New("expression is required")
	}
	if strings.HasPrefix(trimmed, "TZ=") || strings.HasPrefix(trimmed, "CRON_TZ=") {
		return nil, errors.New("TZ= and CRON_TZ= prefixes are not supported; schedules use the host local time zone")
	}
	return parseStandard(trimmed)
}

func parseStandard(expr string) (schedule cron.Schedule, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			schedule, err = nil, fmt.Errorf("parser panic: %v", recovered)
		}
	}()
	return cron.ParseStandard(expr)
}

// Register adds or replaces the schedule named name. When name is already
// registered, the existing job and its busy flag are kept so an in-flight
// invocation still blocks an overlap with the new schedule.
func (s *Scheduler) Register(name, expr string, run func() error) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("scheduled task name is required")
	}
	parsed, err := parseSchedule(expr)
	if err != nil {
		return fmt.Errorf("invalid schedule %q: %w", expr, err)
	}
	j, exists := s.jobs[name]
	if exists {
		s.cron.Remove(j.id)
	} else {
		j = &job{}
		s.jobs[name] = j
	}
	j.id = s.cron.Schedule(parsed, cron.FuncJob(func() { s.fire(name, j, run) }))
	return nil
}

// Unregister removes the schedule named name and reports whether it existed.
func (s *Scheduler) Unregister(name string) bool {
	j, exists := s.jobs[name]
	if !exists {
		return false
	}
	s.cron.Remove(j.id)
	delete(s.jobs, name)
	return true
}

// Stop stops the scheduler and waits for running callbacks until ctx is done.
func (s *Scheduler) Stop(ctx context.Context) {
	stop := s.cron.Stop()
	select {
	case <-stop.Done():
	case <-ctx.Done():
	}
}

func (s *Scheduler) fire(name string, j *job, run func() error) {
	if !j.busy.CompareAndSwap(false, true) {
		s.logger.Warn("scheduled task still running; skipping", "task", name)
		return
	}
	defer j.busy.Store(false)
	started := time.Now()
	s.logger.Info("scheduled task started", "task", name)
	err := run()
	duration := time.Since(started)
	if err != nil {
		s.logger.Error("scheduled task failed", "task", name, "duration", duration, "error", err)
		return
	}
	s.logger.Info("scheduled task finished", "task", name, "duration", duration)
}
