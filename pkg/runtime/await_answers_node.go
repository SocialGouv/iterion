package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// awaitAnswersPollInterval is the store re-check cadence while an
// await_answers node is parked. The in-process answersBell wakes it
// immediately for same-process answers; the poll is the cross-process
// net (CLI answering the store while the engine runs elsewhere).
// A package var (not const) so tests can retune it via
// SetAwaitAnswersPollInterval; production callers never touch it.
var awaitAnswersPollInterval = 5 * time.Second

// SetAwaitAnswersPollInterval overrides the await_answers store-recheck
// cadence and returns the previous value so the caller can restore it.
// TEST-ONLY: a stress test that must isolate the doorbell path from the
// fallback poll pushes the interval far out; a stress test on the
// fallback poll itself pushes it in. Nothing outside tests should call
// this — the production value is deliberately conservative for the
// cross-process CLI-answer path.
func SetAwaitAnswersPollInterval(d time.Duration) time.Duration {
	prev := awaitAnswersPollInterval
	awaitAnswersPollInterval = d
	return prev
}

// awaitAsyncAnswers is the body of an await_answers node (ADR-081): a
// level-triggered wait until no pending Kind=async interaction remains
// for the From node (or the whole run), bounded by the mandatory
// timeout. On success it returns {answers: [...]} with every answered
// async question in scope.
func (e *Engine) awaitAsyncAnswers(ctx context.Context, rs *runState, nodeID string, an *ir.AwaitAnswersNode) (map[string]any, error) {
	deadline := time.NewTimer(an.Timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(awaitAnswersPollInterval)
	defer ticker.Stop()

	for {
		// Arm the doorbell BEFORE checking the store. A ring landing after
		// this arm closes `bell` and wakes the select below; a ring landing
		// before it is caught by the store check that follows (the answer
		// is already persisted). The inverse order (check-then-arm) has a
		// race window: a ring in the [check … arm] gap creates a fresh
		// channel that the arm returns, and the select then blocks on that
		// fresh channel until the poll ticker fires — up to a full poll
		// interval of extra latency, and on a heavily-loaded pod where the
		// deadline collides with the ticker (same duration for the fixture
		// timeout AND the poll cadence) the deadline can win the select
		// and time the branch out on an answered question.
		bell := e.answersBell.wait()

		pending, err := store.ListPendingAsyncInteractions(ctx, e.store, rs.runID, an.From)
		if err != nil {
			return nil, &RuntimeError{
				Code:    ErrCodeExecutionFailed,
				Message: fmt.Sprintf("await_answers %q: list pending async interactions: %v", nodeID, err),
				NodeID:  nodeID,
				Cause:   err,
			}
		}
		if len(pending) == 0 {
			return e.awaitAnswersOutput(ctx, rs.runID, nodeID, an.From)
		}

		select {
		case <-bell:
		case <-ticker.C:
		case <-deadline.C:
			return nil, &RuntimeError{
				Code:    ErrCodeTimeout,
				Message: fmt.Sprintf("await_answers %q: %d async question(s) still unanswered after %s: %s", nodeID, len(pending), an.Timeout, pendingSummary(pending)),
				NodeID:  nodeID,
				Hint:    "answer the pending questions in the studio (or via `iterion run answer-interaction`), raise the timeout, or resume the run once answered",
			}
		case <-ctx.Done():
			return nil, &RuntimeError{
				Code:    ErrCodeCancelled,
				Message: fmt.Sprintf("await_answers %q: cancelled while waiting for async answers", nodeID),
				NodeID:  nodeID,
				Cause:   ctx.Err(),
			}
		}
	}
}

// awaitAnswersOutput builds the node output once nothing is pending:
// {answers: [{interaction_id, node, question, answer}, …]}, oldest first.
func (e *Engine) awaitAnswersOutput(ctx context.Context, runID, nodeID, from string) (map[string]any, error) {
	answered, err := store.ListAnsweredAsyncInteractions(ctx, e.store, runID, from)
	if err != nil {
		return nil, &RuntimeError{
			Code:    ErrCodeExecutionFailed,
			Message: fmt.Sprintf("await_answers %q: list answered async interactions: %v", nodeID, err),
			NodeID:  nodeID,
			Cause:   err,
		}
	}
	answers := make([]any, 0, len(answered))
	for _, in := range answered {
		answers = append(answers, map[string]any{
			"interaction_id": in.ID,
			"node":           in.NodeID,
			"question":       model.AsyncQuestionText(in),
			"answer":         model.AsyncAnswerText(in),
		})
	}
	return map[string]any{"answers": answers}, nil
}

func pendingSummary(pending []*store.Interaction) string {
	parts := make([]string, 0, len(pending))
	for _, in := range pending {
		parts = append(parts, in.ID)
	}
	return strings.Join(parts, ", ")
}

// execAwaitAnswers runs an await_answers node on the main loop through the
// shared special-node envelope.
func (e *Engine) execAwaitAnswers(ctx context.Context, rs *runState, nodeID string, an *ir.AwaitAnswersNode) (string, error) {
	return e.execSpecialNode(rs, nodeID, "await_answers", an,
		map[string]any{"from": an.From},
		func() (map[string]any, error) { return e.awaitAsyncAnswers(ctx, rs, nodeID, an) },
		nil,
	)
}

// handleAwaitEscalation handles the await_answers TOOL escalation
// (ADR-081): the agent asked to sync while questions were pending. The
// store is re-checked first — answers may have landed during the
// escalation round-trip — and only a genuinely-still-pending state
// pauses the run.
func (e *Engine) handleAwaitEscalation(ctx context.Context, rs *runState, nodeID string, node ir.Node, ni *model.ErrNeedsInteraction, depth int) error {
	pending, err := store.ListPendingAsyncInteractions(ctx, e.store, rs.runID, nodeID)
	if err != nil {
		return e.failRunWithCheckpoint(rs, nodeID,
			fmt.Sprintf("await_answers escalation on node %q: list pending async interactions: %v", nodeID, err))
	}

	if len(pending) == 0 {
		// Everything answered while the escalation was in flight: skip
		// the pause entirely and answer the await tool_use immediately.
		text, cerr := model.CollectAsyncAnswersText(ctx, e.store, rs.runID, nodeID)
		if cerr != nil {
			return e.failRunWithCheckpoint(rs, nodeID,
				fmt.Sprintf("await_answers escalation on node %q: collect answers: %v", nodeID, cerr))
		}
		e.logger.Info("await_answers on node %q: all questions answered during escalation — resuming without pause", nodeID)
		return e.reInvokeBackend(ctx, rs, nodeID, node, ni, map[string]any{delegate.AskUserQuestionKey: text}, depth)
	}

	// Refresh the pause payload with the live pending set (some questions
	// may have been answered since the backend snapshot).
	refs := make([]delegate.PendingAsync, 0, len(pending))
	for _, in := range pending {
		refs = append(refs, delegate.PendingAsync{InteractionID: in.ID, Question: model.AsyncQuestionText(in)})
	}
	if ni.Questions == nil {
		ni.Questions = map[string]any{}
	}
	ni.Questions[delegate.AwaitPendingInteractionsKey] = delegate.AwaitPendingToQuestions(refs)
	return e.pauseForBackendInteraction(rs, nodeID, ni)
}

// fanOutAwaitAnswers applies the operator's answers of an await-pause
// resume onto the original async interaction records: answers keyed by
// interaction ID are written through store.AnswerInteraction (already-
// answered records — e.g. answered live via the per-interaction endpoint
// — are left untouched), an interaction_answered event fires per record,
// and any not-yet-delivered node-scoped answer message that would now
// duplicate the resume payload is cancelled. It returns the answers map
// enriched with the canonical collected-answers text under
// AskUserQuestionKey (the ResumeAnswer the backend re-injects), or an
// explicit error when questions remain unanswered.
func (e *Engine) fanOutAwaitAnswers(ctx context.Context, runID, nodeID string, refs []delegate.PendingAsync, answers map[string]any) (map[string]any, error) {
	for _, ref := range refs {
		v, ok := answers[ref.InteractionID]
		if !ok {
			continue
		}
		in, err := store.AnswerInteraction(ctx, e.store, runID, ref.InteractionID, map[string]any{delegate.AskUserQuestionKey: v})
		if err != nil {
			if errors.Is(err, store.ErrInteractionAlreadyAnswered) {
				continue // answered live via the per-interaction endpoint — keep that answer
			}
			return nil, fmt.Errorf("runtime: fan out await answer onto %s: %w", ref.InteractionID, err)
		}
		_ = e.emit(ctx, runID, store.EventInteractionAnswered, in.NodeID, map[string]any{
			"interaction_id": in.ID,
			"async":          true,
			"answer":         fmt.Sprintf("%v", v),
		})
	}

	// Every referenced question must now be answered — resuming with
	// unanswered questions would hand the agent a partial payload.
	pending, err := store.ListPendingAsyncInteractions(ctx, e.store, runID, nodeID)
	if err != nil {
		return nil, fmt.Errorf("runtime: re-check pending async interactions: %w", err)
	}
	if len(pending) > 0 {
		return nil, fmt.Errorf("runtime: cannot resume — %d async question(s) still unanswered: %s (answer each pending question, keyed by its interaction id, or via POST /api/runs/{id}/interactions/{iid}/answer)", len(pending), pendingSummary(pending))
	}

	e.cancelSupersededAnswerMessages(ctx, runID, nodeID)

	text, err := model.CollectAsyncAnswersText(ctx, e.store, runID, nodeID)
	if err != nil {
		return nil, fmt.Errorf("runtime: collect async answers for resume: %w", err)
	}
	answers[delegate.AskUserQuestionKey] = text
	return answers, nil
}

// cancelSupersededAnswerMessages cancels still-queued node-scoped answer
// messages for the asking node: the resume payload (ResumeAnswer) already
// carries every answer, so delivering them again through the inbox would
// duplicate content. Delivered/consumed messages are left alone. Errors
// are logged, not fatal — a duplicate delivery is benign, a blocked
// resume is not.
func (e *Engine) cancelSupersededAnswerMessages(ctx context.Context, runID, nodeID string) {
	pendingMsgs, err := e.store.LoadPendingQueuedMessages(ctx, runID)
	if err != nil {
		e.logger.Warn("await resume: load queued messages for %s: %v", runID, err)
		return
	}
	for _, m := range pendingMsgs {
		if m.NodeID != nodeID || m.InteractionID == "" {
			continue
		}
		if err := e.store.UpdateQueuedMessageStatus(ctx, runID, m.ID, store.QueuedMessageStatusCancelled, store.QueuedMessageStatusQueued); err != nil {
			continue
		}
		store.StampQueuedTransition(&m, store.QueuedMessageStatusCancelled, time.Now().UTC())
		store.PublishInboxEvent(ctx, e.store, e.onEvent, store.EventUserMessageCancelled, runID, m)
	}
}
