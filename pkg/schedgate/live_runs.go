package schedgate

import (
	"context"

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
	if s == nil || scheduleID == "" {
		return nil
	}
	ids, err := s.ListRunsBySchedule(ctx, scheduleID)
	if err != nil {
		if logger != nil {
			logger.Warn("schedgate: list runs for schedule %q: %v (treating as no live runs)", scheduleID, err)
		}
		return nil
	}
	var live []string
	for _, id := range ids {
		r, err := s.LoadRun(ctx, id)
		if err != nil {
			if logger != nil {
				logger.Warn("schedgate: load run %s: %v (skipped from overlap count)", id, err)
			}
			continue
		}
		if !r.Status.IsTerminal() {
			live = append(live, id)
		}
	}
	return live
}
