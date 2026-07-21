package runview

import (
	"context"
	"errors"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Async human interaction (ADR-081) — the service surface for answering
// a pending ask_user_async question, at ANY point of the run lifecycle
// (running, paused, even after the asking node moved on).

// ErrInteractionNotAsync reports an answer targeting a non-async
// interaction (blocking pauses are answered via /answer-human). Maps to 409.
var ErrInteractionNotAsync = errors.New("runview: interaction is not an async question")

// ErrInteractionNotFound reports an unknown interaction ID. Maps to 404.
var ErrInteractionNotFound = errors.New("runview: interaction not found")

// AnswerInteractionResult reports what happened to the answer.
type AnswerInteractionResult struct {
	RunID         string `json:"run_id"`
	InteractionID string `json:"interaction_id"`
	// Queued is true when the answer was queued for mid-run delivery to
	// the asking node (the run keeps executing).
	Queued bool `json:"queued"`
	// Resumed is true when this answer completed the pending set of an
	// await-paused run and the run was auto-resumed.
	Resumed bool `json:"resumed"`
}

// AnswerInteractionCtx records the operator's answer on a pending async
// interaction, delivers it to the asking node's message queue, wakes any
// parked await_answers node, and — when the run is paused on an
// await_answers escalation whose pending set is now fully answered —
// auto-resumes the run.
//
// answer is the operator's reply text (option id or free text).
func (s *Service) AnswerInteractionCtx(ctx context.Context, runID, interactionID, answer string) (*AnswerInteractionResult, error) {
	r, err := s.LoadRunCtx(ctx, runID)
	if err != nil {
		return nil, err
	}
	in, err := s.store.LoadInteraction(ctx, runID, interactionID)
	if err != nil {
		return nil, fmt.Errorf("%w: %s/%s: %v", ErrInteractionNotFound, runID, interactionID, err)
	}
	if in.Kind != store.InteractionKindAsync {
		return nil, fmt.Errorf("%w: %s has kind %q", ErrInteractionNotAsync, interactionID, in.Kind)
	}

	answered, err := store.AnswerInteraction(ctx, s.store, runID, interactionID, map[string]any{delegate.AskUserQuestionKey: answer})
	if err != nil {
		return nil, err // ErrInteractionAlreadyAnswered maps to 409 at the HTTP layer
	}
	s.publishRunEvent(ctx, runID, store.Event{
		Type:   store.EventInteractionAnswered,
		RunID:  runID,
		NodeID: answered.NodeID,
		Data: map[string]any{
			"interaction_id": interactionID,
			"async":          true,
			"answer":         answer,
		},
	})

	result := &AnswerInteractionResult{RunID: runID, InteractionID: interactionID}

	// Deliver to the asking node's inbox (node-scoped: a late answer can
	// never leak into an unrelated node). Delivered at the node's next
	// turn boundary by the existing inbox drains; superseded copies are
	// cancelled by the await-resume path.
	text := model.FormatAsyncAnswerMessage(interactionID, model.AsyncQuestionText(answered), answer)
	if _, qerr := s.QueueMessage(ctx, runID, text, WithMessageNode(answered.NodeID)); qerr != nil {
		return nil, fmt.Errorf("runview: answer recorded but queueing delivery failed: %w", qerr)
	}
	result.Queued = true

	// Wake a parked await_answers node in this process immediately.
	s.runLogsMu.RLock()
	eng := s.runEngines[runID]
	s.runLogsMu.RUnlock()
	if eng != nil {
		eng.NotifyInteractionAnswered()
	}

	// Auto-resume an await-paused run once nothing is pending: the resume
	// fan-out path re-collects the answers (tolerating already-answered
	// records) and injects the aggregated text as the ResumeAnswer.
	if r.Status == store.RunStatusPausedWaitingHuman && r.Checkpoint != nil && r.Checkpoint.InteractionQuestions != nil {
		refs := delegate.ParseAwaitPending(r.Checkpoint.InteractionQuestions[delegate.AwaitPendingInteractionsKey])
		if len(refs) > 0 && r.Checkpoint.NodeID == answered.NodeID {
			pending, perr := store.ListPendingAsyncInteractions(ctx, s.store, runID, answered.NodeID)
			if perr != nil {
				return nil, fmt.Errorf("runview: answer recorded but pending re-check failed: %w", perr)
			}
			if len(pending) == 0 {
				if _, rerr := s.Resume(ctx, ResumeSpec{RunID: runID, FilePath: r.FilePath, Answers: map[string]any{}}); rerr != nil {
					return nil, fmt.Errorf("runview: answer recorded but auto-resume failed: %w", rerr)
				}
				result.Resumed = true
			}
		}
	}
	return result, nil
}

// publishRunEvent appends an event to the run log and fans it out to the
// live broker (nil-safe on both).
func (s *Service) publishRunEvent(ctx context.Context, runID string, evt store.Event) {
	persisted, err := s.store.AppendEvent(ctx, runID, evt)
	if err != nil || persisted == nil {
		return
	}
	if s.broker != nil {
		s.broker.Publish(*persisted)
	}
}

// PendingAsyncInteractions lists the run's pending async questions
// (studio card + CLI inspect surface).
func (s *Service) PendingAsyncInteractions(ctx context.Context, runID string) ([]*store.Interaction, error) {
	if _, err := s.LoadRunCtx(ctx, runID); err != nil {
		return nil, err
	}
	return store.ListPendingAsyncInteractions(ctx, s.store, runID, "")
}
