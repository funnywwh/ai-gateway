package backup

import (
	"context"
	"time"
)

// Scheduler runs backups on a cron expression.
type Scheduler struct {
	manager  *Manager
	schedule *Schedule
	// Tick is how often the scheduler checks the clock; tests shrink it.
	Tick time.Duration
}

// NewScheduler parses the expression and prepares the scheduler. An empty expression
// means \"do not run automatically\".
func NewScheduler(manager *Manager, expression string) (*Scheduler, error) {
	if manager == nil {
		return nil, nil
	}
	schedule, err := ParseSchedule(expression)
	if err != nil {
		return nil, err
	}
	manager.SetSchedule(schedule)
	return &Scheduler{manager: manager, schedule: schedule, Tick: 30 * time.Second}, nil
}

// NextRun reports the next scheduled instant after now.
func (s *Scheduler) NextRun(after time.Time) time.Time {
	if s == nil || s.schedule == nil {
		return time.Time{}
	}
	return s.schedule.Next(after)
}

// Start runs backups until the context is cancelled. The next instant is recomputed
// after every run, so a missed window (process down) is simply skipped, never queued.
func (s *Scheduler) Start(ctx context.Context, log logger) {
	if s == nil {
		return
	}
	tick := s.Tick
	if tick <= 0 {
		tick = 30 * time.Second
	}
	go func() {
		timer := time.NewTicker(tick)
		defer timer.Stop()
		next := s.schedule.Next(time.Now().UTC())
		log.Info("backup scheduler started", "expression", s.schedule.String(),
			"next_run", next.UTC().Format(time.RFC3339))
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-timer.C:
				if next.IsZero() || now.UTC().Before(next) {
					continue
				}
				if _, err := s.manager.Run(ctx, "cron"); err != nil {
					log.Error("scheduled backup failed", "err", err)
				}
				next = s.schedule.Next(time.Now().UTC())
				log.Info("next backup scheduled", "next_run", next.UTC().Format(time.RFC3339))
			}
		}
	}()
}

// logger is the subset of slog the scheduler needs.
type logger interface {
	Info(msg string, args ...any)
	Error(msg string, args ...any)
}
