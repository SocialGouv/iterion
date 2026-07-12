package runview

import (
	"context"
	"errors"
	"time"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// emitRunOutcome publishes a run-outcome trigger.Event when a run leaves the
// engine — the "runned by iterion" source that lets a finished/failed run fire
// downstream runs (pipelines, on-failure escalation) and a paused run mark its
// board card "awaiting input". The kind spans terminal outcomes
// (run.finished/failed/cancelled) AND the non-terminal run.paused. It is a
// best-effort no-op unless an event publisher is wired, and never blocks the
// run goroutine on a slow consumer (the bus fan-out is itself lossy).
//
// The kind is derived from bodyErr (the engine already wrote the authoritative
// status to the run record, re-read here to enrich the event with repo/bot for
// matching and templating). A load failure still emits the bare event so a
// downstream subscription keyed only on source/kind/bot still fires.
func (s *Service) emitRunOutcome(runID string, bodyErr error) {
	if s.eventPublisher == nil {
		return
	}
	// A pause is NOT a terminal failure. Match it BEFORE the bodyErr!=nil arm
	// so a run that suspends on a human node (ErrRunPaused) or an operator
	// soft-pause (ErrRunPausedOperator) emits run.paused, not run.failed. This
	// is load-failure-resilient: it holds even when the persisted-status enrich
	// below can't read the run.
	kind := trigger.KindRunFinished
	switch {
	case errors.Is(bodyErr, runtime.ErrRunCancelled):
		kind = trigger.KindRunCancelled
	case errors.Is(bodyErr, runtime.ErrRunPaused), errors.Is(bodyErr, runtime.ErrRunPausedOperator):
		kind = trigger.KindRunPaused
	case bodyErr != nil:
		kind = trigger.KindRunFailed
	}

	fctx := store.WithoutTenantFilter(context.Background())
	var repo, botID, status, name, nodeID, interactionID string
	if r, err := s.store.LoadRun(fctx, runID); err == nil && r != nil {
		repo = r.ProjectPath
		botID = r.BotID
		status = string(r.Status)
		name = r.Name
		if name == "" {
			name = r.WorkflowName
		}
		// Trust the persisted status over the derived kind when it is
		// unambiguous (the engine is the source of truth).
		switch {
		case r.Status == store.RunStatusFinished:
			kind = trigger.KindRunFinished
		case r.Status == store.RunStatusFailed, r.Status == store.RunStatusFailedResumable:
			kind = trigger.KindRunFailed
		case r.Status == store.RunStatusCancelled:
			kind = trigger.KindRunCancelled
		case r.Status.IsPaused():
			kind = trigger.KindRunPaused
		}
		// A paused run carries the node + pending interaction on its
		// checkpoint; surface both so a board projection can pinpoint the
		// paused node and render the answer affordance.
		if r.Checkpoint != nil {
			nodeID = r.Checkpoint.NodeID
			interactionID = r.Checkpoint.InteractionID
		}
	}

	payload := map[string]any{"bot_id": botID, "status": status, "run_id": runID}
	if nodeID != "" {
		payload["node_id"] = nodeID
	}
	if interactionID != "" {
		payload["interaction_id"] = interactionID
	}
	ev := trigger.Event{
		ID:         "run:" + runID,
		Source:     trigger.SourceRun,
		Kind:       kind,
		Repo:       repo,
		Subject:    trigger.Subject{Type: "run", ID: runID, Title: name, State: status},
		Actor:      botID,
		Payload:    payload,
		OccurredAt: time.Now().UTC(),
	}
	_ = s.eventPublisher.Publish(fctx, ev)
}
