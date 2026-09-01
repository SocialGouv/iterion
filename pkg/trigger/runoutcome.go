package trigger

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// BuildRunOutcome derives the run.<outcome> Event for a run leaving the
// engine — the "runned by iterion" source shared by the in-process runview
// emitter and the cloud runner. The kind spans terminal outcomes
// (run.finished/failed/cancelled) AND the non-terminal run.paused.
//
// The kind is first classified from bodyErr, then overridden by the
// persisted run status when it is unambiguous (the engine is the source of
// truth; the bodyErr arm keeps the event load-failure-resilient). The event
// carries the run's TenantID and the launching owner (payload "owner_id")
// so tenant-scoped consumers (notifications, board projections) can target
// it without a second store read.
//
// The event ID is distinct per outcome episode: a run that pauses, resumes
// and pauses again must not dedup against its earlier pause, so the pending
// interaction id (or the status) is folded into the key.
// RunOutcomeEventID derives the per-episode event ID: the pending
// interaction when the run is paused on one, else the status — plus the
// run's updated_at second, which moves on every pause/terminal
// transition. The timestamp is what makes REPEAT episodes distinct: a
// failed_resumable run resumed and failing again, or a review-gate node
// re-pausing on the same interaction id, must notify again rather than
// dedup against the earlier episode. Exported so the usernotify sweep can
// derive the episode key from a run listing without loading each run;
// truncation to the second keeps the key stable across the stores'
// differing timestamp precisions.
func RunOutcomeEventID(runID, status, interactionID string, updatedAt time.Time) string {
	episode := status
	if interactionID != "" && store.RunStatus(status).IsPaused() {
		episode = interactionID
	}
	if episode != "" && !updatedAt.IsZero() {
		episode += ":" + strconv.FormatInt(updatedAt.Truncate(time.Second).Unix(), 10)
	}
	id := "run:" + runID
	if episode != "" {
		id += ":" + episode
	}
	return id
}

func BuildRunOutcome(ctx context.Context, rs store.RunStore, runID string, bodyErr error) Event {
	// A pause is NOT a terminal failure. Match it BEFORE the bodyErr!=nil arm
	// so a run that suspends on a human node (ErrRunPaused) or an operator
	// soft-pause (ErrRunPausedOperator) yields run.paused, not run.failed.
	kind := KindRunFinished
	switch {
	case errors.Is(bodyErr, runtime.ErrRunCancelled):
		kind = KindRunCancelled
	case errors.Is(bodyErr, runtime.ErrRunPaused), errors.Is(bodyErr, runtime.ErrRunPausedOperator):
		kind = KindRunPaused
	case bodyErr != nil:
		kind = KindRunFailed
	}

	fctx := store.WithoutTenantFilter(ctx)
	var repo, botID, status, name, nodeID, interactionID, tenantID, ownerID string
	var updatedAt time.Time
	if r, err := rs.LoadRun(fctx, runID); err == nil && r != nil {
		updatedAt = r.UpdatedAt
		repo = r.ProjectPath
		botID = r.BotID
		status = string(r.Status)
		tenantID = r.TenantID
		ownerID = r.OwnerID
		name = r.Name
		if name == "" {
			name = r.WorkflowName
		}
		// Trust the persisted status over the derived kind when it is
		// unambiguous (the engine is the source of truth).
		switch {
		case r.Status == store.RunStatusFinished:
			kind = KindRunFinished
		case r.Status == store.RunStatusFailed, r.Status == store.RunStatusFailedResumable:
			kind = KindRunFailed
		case r.Status == store.RunStatusCancelled:
			kind = KindRunCancelled
		case r.Status.IsPaused():
			kind = KindRunPaused
		}
		// The checkpoint's NodeID is the outcome's anchor — the paused
		// node on a pause, the failing node on a failure ("The run
		// failed at node X" in usernotify) — valid on every status. The
		// interaction id is different: a consumable pause pointer,
		// status-gated because the checkpoint survives every transition
		// now (ADR-095) — a terminal outcome must not deep-link to an
		// answered form.
		if r.Checkpoint != nil {
			nodeID = r.Checkpoint.NodeID
			if r.Status.IsPaused() {
				interactionID = r.Checkpoint.InteractionID
			}
		}
	}

	payload := map[string]any{"bot_id": botID, "status": status, "run_id": runID}
	if nodeID != "" {
		payload["node_id"] = nodeID
	}
	if interactionID != "" {
		payload["interaction_id"] = interactionID
	}
	if ownerID != "" {
		payload["owner_id"] = ownerID
	}

	return Event{
		ID:         RunOutcomeEventID(runID, status, interactionID, updatedAt),
		Source:     SourceRun,
		Kind:       kind,
		TenantID:   tenantID,
		Repo:       repo,
		Subject:    Subject{Type: "run", ID: runID, Title: name, State: status},
		Actor:      botID,
		Payload:    payload,
		OccurredAt: time.Now().UTC(),
	}
}
