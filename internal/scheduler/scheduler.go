// Package scheduler runs periodic scans and prunes old probe data.
package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/vefgh/botchecker/internal/scanner"
	"github.com/vefgh/botchecker/internal/store"
)

type Scheduler struct {
	sc            *scanner.Scanner
	st            *store.Store
	log           *slog.Logger
	interval      time.Duration
	initialDelay  time.Duration
	retentionDays int
}

func New(sc *scanner.Scanner, st *store.Store, log *slog.Logger, interval, initialDelay time.Duration, retentionDays int) *Scheduler {
	return &Scheduler{
		sc: sc, st: st, log: log,
		interval: interval, initialDelay: initialDelay, retentionDays: retentionDays,
	}
}

// Start launches the background loops and returns immediately. Scans are
// skipped rather than queued while one is already running, so a slow scan
// cannot pile up behind the ticker.
func (s *Scheduler) Start(ctx context.Context) {
	go s.pruneLoop(ctx)

	if s.interval <= 0 {
		s.log.Info("scheduled scans disabled", "reason", "SCAN_INTERVAL is zero")
		return
	}
	go s.scanLoop(ctx)
}

func (s *Scheduler) scanLoop(ctx context.Context) {
	if s.initialDelay > 0 {
		t := time.NewTimer(s.initialDelay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}

	s.runOnce(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runOnce(ctx)
		}
	}
}

func (s *Scheduler) runOnce(ctx context.Context) {
	id, err := s.sc.Start(ctx, scanner.TriggerScheduled)
	switch {
	case errors.Is(err, scanner.ErrBusy):
		s.log.Warn("skipping scheduled scan", "reason", "previous scan still running")
	case err != nil:
		s.log.Error("could not start scheduled scan", "err", err)
	default:
		s.log.Info("scheduled scan started", "scan_id", id)
	}
}

func (s *Scheduler) pruneLoop(ctx context.Context) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	s.prune()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.prune()
		}
	}
}

func (s *Scheduler) prune() {
	cutoff := time.Now().AddDate(0, 0, -s.retentionDays)
	n, err := s.st.Prune(cutoff)
	if err != nil {
		s.log.Error("prune failed", "err", err)
		return
	}
	if n > 0 {
		s.log.Info("pruned old probe data", "results_deleted", n, "older_than", cutoff.Format(time.DateOnly))
	}
}
