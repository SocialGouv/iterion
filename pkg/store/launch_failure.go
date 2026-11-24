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
	cause = launchFailureCause(cause)
	if err := s.UpdateRunStatusCoded(ctx, runID, RunStatusFailed, cause, FailureLaunchFailed); err != nil {
		return err
	}
	return appendLaunchFailedEvent(ctx, s, runID, cause)
}

// FailRunAtLaunchIf is FailRunAtLaunch for a launch that cannot tell whether
// it handed the run on: the run is ended only while its status is still one
// of expectedFrom — the status the launch persisted — and reports whether it
// was. A queue publish can report an error after its message landed, and a
// runner that claimed that message owns the run: an unconditional write
// would mark a run that executes as one that never left its launch path.
// The timeline is written only with the transition.
func FailRunAtLaunchIf(ctx context.Context, s RunStore, runID, cause string, expectedFrom ...RunStatus) (bool, error) {
	if s == nil {
		return false, fmt.Errorf("store: FailRunAtLaunchIf: no store")
	}
	if len(expectedFrom) == 0 {
		return false, fmt.Errorf("store: FailRunAtLaunchIf: run %s: no expected status — a launch rollback must name the status it persisted", runID)
	}
	cause = launchFailureCause(cause)
	ended, err := s.UpdateRunStatusIfCoded(ctx, runID, RunStatusFailed, cause, FailureLaunchFailed, expectedFrom)
	if err != nil || !ended {
		return false, err
	}
	return true, appendLaunchFailedEvent(ctx, s, runID, cause)
}

// launchFailureCause never leaves a launch failure without words.
func launchFailureCause(cause string) string {
	if cause == "" {
		return "the run never left its launch path"
	}
	return cause
}

// appendLaunchFailedEvent writes the timeline half of a launch failure.
func appendLaunchFailedEvent(ctx context.Context, s RunStore, runID, cause string) error {
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
