package schedgate

import (
	"context"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ScheduleRunLister is the narrow slice of store.RunStore the overlap
// gate needs. Both the filesystem and Mongo stores satisfy it (via
// ListRunsBySchedule); kept as a local interface so tests inject fakes
// and this package stays store-agnostic.
type ScheduleRunLister interface {
	ListRunsBySchedule(ctx context.Context, scheduleID string) ([]string, error)
	LoadRun(ctx context.Context, id string) (*store.Run, error)
}

// LiveRunsForSchedule returns the IDs of runs stamped with this
// schedule's provenance whose status is non-terminal, preserving the
// store's created_at-ascending order (so index 0 is the oldest — the
// deterministic "blocking run" for audit).
//
// A run that fails to load is logged and skipped rather than blocking
// the tick: this is a gate, not a data-integrity boundary.
func LiveRunsForSchedule(ctx context.Context, s ScheduleRunLister, scheduleID string, logger *iterlog.Logger) []string {
	live, _ := LiveAndStaleRunsForSchedule(ctx, s, scheduleID, 0, time.Time{}, logger)
	return live
}

// LiveAndStaleRunsForSchedule is the keepalive-aware liveness query. It
// returns two disjoint, order-preserving lists of non-terminal runs
// stamped with this schedule's provenance:
//
//   - live: runs that count against the overlap policy;
//   - stale: running runs whose last progress (UpdatedAt) is older than
//     staleAfter — treated as dead so a keepalive tick relaunches, and
//     returned so the caller can reap them.
//
// When staleAfter <= 0 (the non-keepalive path) no run is ever
// considered stale, so this reduces exactly to the old
// LiveRunsForSchedule behavior. Staleness is gated on RunStatusRunning
// only: a paused run is legitimately idle and must never be reaped.
func LiveAndStaleRunsForSchedule(ctx context.Context, s ScheduleRunLister, scheduleID string, staleAfter time.Duration, now time.Time, logger *iterlog.Logger) (live, stale []string) {
	if s == nil || scheduleID == "" {
		return nil, nil
	}
	ids, err := s.ListRunsBySchedule(ctx, scheduleID)
	if err != nil {
		if logger != nil {
			logger.Warn("schedgate: list runs for schedule %q: %v (treating as no live runs)", scheduleID, err)
		}
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	for _, id := range ids {
		r, err := s.LoadRun(ctx, id)
		if err != nil {
			if logger != nil {
				logger.Warn("schedgate: load run %s: %v (skipped from overlap count)", id, err)
			}
			continue
		}
		if r.Status.IsTerminal() {
			continue
		}
		if staleAfter > 0 && r.Status == store.RunStatusRunning && now.Sub(r.UpdatedAt) > staleAfter {
			stale = append(stale, id)
			continue
		}
		live = append(live, id)
	}
	return live, stale
}
