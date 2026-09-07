package store

import (
	"context"
	"fmt"
)

// FailRunAtLaunch is the one place a run that never left its launch path is
// ended. A row exists (the launch persisted it) but no executor will ever
// claim it: the queue publish failed, the detached runner would not start,
// the queued pipeline could not be picked up.
//
// It exists as a chokepoint because each of those sites wrote the status
// its own way and NONE of them wrote the timeline. A terminal status with
// no event is the same defect as a terminal status with no error: a
// consumer that triages by the tree — the run console, a headless outcome
// router — reads a run that simply stopped, with nothing to act on
// (measured 2026-09-05: `status = failed`, `final_error = None`, three
// sandbox markers and no `run_failed`).
//
// The status write is what matters; the event is best-effort and reported
// separately, so a store that accepted the transition never has it undone
// by a timeline hiccup.
func FailRunAtLaunch(ctx context.Context, s RunStore, runID, cause string) error {
	if s == nil {
		return fmt.Errorf("store: FailRunAtLaunch: no store")
	}
	if cause == "" {
		cause = "the run never left its launch path"
	}
	if err := s.UpdateRunStatusCoded(ctx, runID, RunStatusFailed, cause, FailureLaunchFailed); err != nil {
		return err
	}
	if _, err := s.AppendEvent(ctx, runID, Event{
		Type:  EventRunFailed,
		RunID: runID,
		Data: map[string]any{
			"error": cause,
			"code":  string(FailureLaunchFailed),
			"phase": "launch",
			"hint":  "the run was persisted but never handed to an executor; re-launch it — resuming this row has nothing to resume from",
		},
	}); err != nil {
		return fmt.Errorf("store: FailRunAtLaunch: run %s recorded failed, but its run_failed event was not: %w", runID, err)
	}
	return nil
}
