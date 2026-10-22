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
//
// One outcome does NOT use it: a run parked on a provider usage window, whose
// episode is the WALL it waits on (RunUsageParkEventID). There, updated_at
// moves with every piece of retry bookkeeping while nothing new has happened
// to the run.
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

// RunUsageParkEventID is the episode of a run PARKED ON A PROVIDER WALL
// (store.UsageParkWall): the run and the wall, and nothing else.
//
// A usage park is not an episode per transition. One wall collects a park, a
// pull-forward probe, a free re-arm per probe of the budget and a charged park
// per attempt, each rewriting the row and so moving updated_at — so the
// per-transition key made each of them a fresh episode and pushed one message
// per bookkeeping write to the run's owner (about twenty-five on a weekly wall
// at the shipped probe budget, every one of them saying the run had failed).
// Keyed on the wall, the claim that dedups the bus and the sweep dedups them
// too, and a wall the provider MOVES is a new episode because it is news.
//
// Truncated to the second for RunOutcomeEventID's reason: the stores hold
// instants at different precisions, and the sweep derives this key from a
// listing while the event path derives it from the document.
func RunUsageParkEventID(runID string, wall time.Time) string {
	return "run:" + runID + ":usage_wall:" + strconv.FormatInt(wall.UTC().Truncate(time.Second).Unix(), 10)
}

// RunHealthEventID is the stable identity of a persisted run-health event.
// Health events already carry a monotonic per-run store sequence, which is a
// stronger episode key than a wall-clock timestamp and stays valid across a
// coordinator restart.
func RunHealthEventID(runID string, seq int64) string {
	return "run:" + runID + ":health:" + strconv.FormatInt(seq, 10)
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
	var bankState store.BankState
	var finalBranch, finalCommit, finalBranchError string
	// usageWall is the provider wall a parked run is waiting on, zero for
	// every other outcome — the fact that makes the episode the WAIT instead
	// of the transition. usageWake is when that row actually comes back,
	// which is NOT the wall: the arming takes the earlier of the wall and a
	// skipped credential's reopening, then applies the jitter, the floor and
	// the max_wait ceiling. The wall identifies the episode; the wake is what
	// a reader is told to expect, so both travel.
	var usageWall, usageWake time.Time
	if r, err := rs.LoadRun(fctx, runID); err == nil && r != nil {
		updatedAt = r.UpdatedAt
		usageWall = r.UsageParkWall()
		if !usageWall.IsZero() && r.RetryState != nil && r.RetryState.RetryAfter != nil {
			usageWake = r.RetryState.RetryAfter.UTC()
		}
		bankState, finalBranch, finalCommit, finalBranchError = r.BankState(), r.FinalBranch, r.FinalCommit, r.FinalBranchError
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
		// interaction id is different: a consumable pause pointer.
		// Stricter than CarriesPausePointer (paused ∨ queued) on
		// purpose — an outcome event for a queued run must not
		// deep-link an answer form that is already in flight; the
		// store's transition normalization is the enforcing layer,
		// this gate is belt-and-braces.
		if r.Checkpoint != nil {
			nodeID = r.Checkpoint.NodeID
			if r.Status.IsPaused() {
				nodeID = r.Checkpoint.PausedNodeID()
				interactionID = r.Checkpoint.InteractionID
			}
		}
	}

	payload := map[string]any{"bot_id": botID, "status": status, "run_id": runID}
	// A usage park carries its WALL, so every consumer keys on the wait
	// rather than on the transition — and reads it from the row that KNOWS
	// it, never re-derived from retry_after (which the jitter, the shared
	// circuit and a probe's pull all move) nor from the order of events.
	if !usageWall.IsZero() {
		payload["usage_park_wall"] = usageWall.UTC().Format(time.RFC3339)
	}
	// The instant the row is armed for. A consumer telling the run's owner
	// when their run comes back has to say THIS one: the wall can be days
	// later than the wake, because the arming takes the earlier of the wall
	// and a skipped credential's reopening (measured: a team key reopening
	// the same afternoon while the platform forfait it fell through to is
	// walled until Monday — 69 hours apart).
	if !usageWake.IsZero() {
		payload["usage_park_resumes_at"] = usageWake.Format(time.RFC3339)
	}
	if bankState != "" {
		payload["bank_state"] = string(bankState)
		payload["final_branch"] = finalBranch
		payload["final_commit"] = finalCommit
		if finalBranchError != "" {
			payload["final_branch_error"] = finalBranchError
		}
	}
	if nodeID != "" {
		payload["node_id"] = nodeID
	}
	if interactionID != "" {
		payload["interaction_id"] = interactionID
	}
	if ownerID != "" {
		payload["owner_id"] = ownerID
	}

	id := RunOutcomeEventID(runID, status, interactionID, updatedAt)
	if !usageWall.IsZero() {
		id = RunUsageParkEventID(runID, usageWall)
	}
	return Event{
		ID:         id,
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
