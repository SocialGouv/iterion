package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// ErrInteractionAlreadyAnswered is returned by AnswerInteraction when the
// target interaction already carries answers — a second answer must surface
// as a conflict, never silently overwrite the first.
var ErrInteractionAlreadyAnswered = errors.New("interaction already answered")

// Async-interaction helpers (ADR-081). These are free functions over the
// RunStore interface — NOT interface methods — so the filesystem and Mongo
// stores share one implementation with identical semantics, built on the
// three primitive interaction methods every store already provides.

// ListPendingAsyncInteractions returns the pending (unanswered) async
// interactions of a run, oldest-first. nodeID filters to questions posted by
// one node; empty means the whole run.
func ListPendingAsyncInteractions(ctx context.Context, rs RunStore, runID, nodeID string) ([]*Interaction, error) {
	return listAsyncInteractions(ctx, rs, runID, nodeID, false)
}

// ListAnsweredAsyncInteractions returns the answered async interactions
// of a run, oldest-first. nodeID filters like the pending variant.
func ListAnsweredAsyncInteractions(ctx context.Context, rs RunStore, runID, nodeID string) ([]*Interaction, error) {
	return listAsyncInteractions(ctx, rs, runID, nodeID, true)
}

// listAsyncInteractions is the shared core: list + load + filter on
// kind/answered-state/node, oldest-first. ListInteractions is ID-ordered
// (fs) / requested_at-ordered (mongo); the sort normalizes so both
// stores agree.
func listAsyncInteractions(ctx context.Context, rs RunStore, runID, nodeID string, answered bool) ([]*Interaction, error) {
	ids, err := rs.ListInteractions(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list interactions for run %s: %w", runID, err)
	}
	var out []*Interaction
	for _, id := range ids {
		in, err := rs.LoadInteraction(ctx, runID, id)
		if err != nil {
			return nil, fmt.Errorf("load interaction %s/%s: %w", runID, id, err)
		}
		if in.Kind != InteractionKindAsync || (in.AnsweredAt != nil) != answered {
			continue
		}
		if nodeID != "" && in.NodeID != nodeID {
			continue
		}
		out = append(out, in)
	}
	sortInteractionsByRequestedAt(out)
	return out, nil
}

// AnswerInteraction records the answers on a pending interaction and stamps
// AnsweredAt. It refuses to overwrite an already-answered interaction
// (ErrInteractionAlreadyAnswered) — the caller decides how to surface the
// conflict. Returns the updated record.
func AnswerInteraction(ctx context.Context, rs RunStore, runID, interactionID string, answers map[string]any) (*Interaction, error) {
	in, err := rs.LoadInteraction(ctx, runID, interactionID)
	if err != nil {
		return nil, err
	}
	if in.AnsweredAt != nil {
		return nil, fmt.Errorf("interaction %s/%s: %w", runID, interactionID, ErrInteractionAlreadyAnswered)
	}
	now := time.Now().UTC()
	in.Answers = answers
	in.AnsweredAt = &now
	if err := rs.WriteInteraction(ctx, in); err != nil {
		return nil, fmt.Errorf("write answered interaction %s/%s: %w", runID, interactionID, err)
	}
	return in, nil
}

func sortInteractionsByRequestedAt(ins []*Interaction) {
	sort.SliceStable(ins, func(i, j int) bool {
		return ins[i].RequestedAt.Before(ins[j].RequestedAt)
	})
}
