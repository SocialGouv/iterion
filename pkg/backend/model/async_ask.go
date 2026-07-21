package model

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Async human interaction (ADR-081) — the executor-side plumbing that
// backs the ask_user_async / await_answers tools. Mirrors the InboxBinder
// pattern: the runtime/runview layer supplies a store-backed binder, the
// executor binds one hook per (run, node) and threads its methods onto
// the delegate.Task as closures.

// AsyncAskHook is one node's async-question surface.
type AsyncAskHook interface {
	// Post persists a pending Kind=async Interaction and emits
	// human_input_requested{async:true}. Returns the interaction ID.
	Post(ctx context.Context, q delegate.AsyncQuestion) (string, error)
	// Pending lists the node's still-unanswered async questions
	// (oldest first).
	Pending(ctx context.Context) ([]delegate.PendingAsync, error)
	// CollectAnswers formats every answered async question of the node
	// into one human-readable block (the await_answers success result).
	CollectAnswers(ctx context.Context) (string, error)
}

// AsyncAskBinder constructs a per-(run,node) AsyncAskHook. Returning nil
// disables async asks for that run (the tools then error explicitly).
type AsyncAskBinder interface {
	BindAsyncAsk(ctx context.Context, runID, nodeID string) AsyncAskHook
}

// WithExecutorAsyncAsk installs the async-ask binder on the executor.
// Tasks built for interaction: async nodes then carry the
// PostAsyncQuestion / PendingAsyncQuestions / CollectAsyncAnswers
// closures consumed by both backends.
func WithExecutorAsyncAsk(b AsyncAskBinder) ClawExecutorOption {
	return func(e *ClawExecutor) { e.asyncAsk = b }
}

// StoreAsyncAskBinder is the production AsyncAskBinder, backed by a
// store.RunStore + an event-broker publish callback (same pair as
// StoreInboxBinder — local mode passes EventBroker.Publish, cloud mode
// nil and the change stream surfaces transitions).
type StoreAsyncAskBinder struct {
	Store   store.RunStore
	Publish func(store.Event)
	// OnPosted, when set, is invoked after each successfully posted
	// question (e.g. to ring a notification surface). Optional.
	OnPosted func(runID, interactionID string)
}

// BindAsyncAsk returns the hook scoped to (runID, nodeID), or nil when
// the binder is not configured for this run.
func (b *StoreAsyncAskBinder) BindAsyncAsk(_ context.Context, runID, nodeID string) AsyncAskHook {
	if b == nil || b.Store == nil || runID == "" || nodeID == "" {
		return nil
	}
	return &storeAsyncAskHook{store: b.Store, publish: b.Publish, onPosted: b.OnPosted, runID: runID, nodeID: nodeID}
}

type storeAsyncAskHook struct {
	store    store.RunStore
	publish  func(store.Event)
	onPosted func(runID, interactionID string)
	runID    string
	nodeID   string
}

func (h *storeAsyncAskHook) Post(ctx context.Context, q delegate.AsyncQuestion) (string, error) {
	question := strings.TrimSpace(q.Question)
	if question == "" {
		return "", fmt.Errorf("ask_user_async: empty question")
	}
	id, err := h.nextInteractionID(ctx)
	if err != nil {
		return "", err
	}
	questions := map[string]any{delegate.AskUserQuestionKey: question}
	delegate.AddAskUserOptionKeys(questions, q.Options, q.AllowFreeText)
	in := &store.Interaction{
		ID:          id,
		RunID:       h.runID,
		NodeID:      h.nodeID,
		Kind:        store.InteractionKindAsync,
		RequestedAt: time.Now().UTC(),
		Questions:   questions,
	}
	if err := h.store.WriteInteraction(ctx, in); err != nil {
		return "", fmt.Errorf("ask_user_async: write interaction %s: %w", id, err)
	}
	evt := store.Event{
		Type:   store.EventHumanInputRequested,
		RunID:  h.runID,
		NodeID: h.nodeID,
		Data: map[string]any{
			"interaction_id": id,
			"questions":      questions,
			"async":          true,
		},
	}
	persisted, err := h.store.AppendEvent(ctx, h.runID, evt)
	if err == nil && persisted != nil && h.publish != nil {
		h.publish(*persisted)
	}
	if h.onPosted != nil {
		h.onPosted(h.runID, id)
	}
	return id, nil
}

func (h *storeAsyncAskHook) Pending(ctx context.Context) ([]delegate.PendingAsync, error) {
	pending, err := store.ListPendingAsyncInteractions(ctx, h.store, h.runID, h.nodeID)
	if err != nil {
		return nil, err
	}
	out := make([]delegate.PendingAsync, 0, len(pending))
	for _, in := range pending {
		out = append(out, delegate.PendingAsync{
			InteractionID: in.ID,
			Question:      AsyncQuestionText(in),
		})
	}
	return out, nil
}

func (h *storeAsyncAskHook) CollectAnswers(ctx context.Context) (string, error) {
	return CollectAsyncAnswersText(ctx, h.store, h.runID, h.nodeID)
}

// CollectAsyncAnswersText formats every ANSWERED async question of a node
// (nodeID empty = whole run) into one human-readable block. Shared by the
// await_answers tool result (both backends) and the runtime's await-node
// output / resume re-injection so the agent always sees one shape.
func CollectAsyncAnswersText(ctx context.Context, rs store.RunStore, runID, nodeID string) (string, error) {
	answered, err := ListAnsweredAsyncInteractions(ctx, rs, runID, nodeID)
	if err != nil {
		return "", err
	}
	if len(answered) == 0 {
		return "No async questions were posted (nothing to wait for).", nil
	}
	var b strings.Builder
	b.WriteString("All posted questions are answered:\n")
	for _, in := range answered {
		fmt.Fprintf(&b, "- [%s] Q: %s — A: %s\n", in.ID, AsyncQuestionText(in), AsyncAnswerText(in))
	}
	return b.String(), nil
}

// ListAnsweredAsyncInteractions returns the answered async interactions of
// a run (nodeID empty = all nodes), oldest-first.
func ListAnsweredAsyncInteractions(ctx context.Context, rs store.RunStore, runID, nodeID string) ([]*store.Interaction, error) {
	ids, err := rs.ListInteractions(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list interactions for run %s: %w", runID, err)
	}
	var answered []*store.Interaction
	for _, id := range ids {
		in, err := rs.LoadInteraction(ctx, runID, id)
		if err != nil {
			return nil, fmt.Errorf("load interaction %s/%s: %w", runID, id, err)
		}
		if in.Kind != store.InteractionKindAsync || in.AnsweredAt == nil {
			continue
		}
		if nodeID != "" && in.NodeID != nodeID {
			continue
		}
		answered = append(answered, in)
	}
	sort.SliceStable(answered, func(i, j int) bool {
		return answered[i].RequestedAt.Before(answered[j].RequestedAt)
	})
	return answered, nil
}

// nextInteractionID allocates <runID>_<nodeID>_async_<n> with n one past
// the highest existing suffix — collision-proof across loop iterations
// and resumes (unlike a per-session counter).
func (h *storeAsyncAskHook) nextInteractionID(ctx context.Context) (string, error) {
	ids, err := h.store.ListInteractions(ctx, h.runID)
	if err != nil {
		return "", fmt.Errorf("ask_user_async: list interactions: %w", err)
	}
	prefix := fmt.Sprintf("%s_%s_async_", h.runID, h.nodeID)
	max := 0
	for _, id := range ids {
		var n int
		if _, err := fmt.Sscanf(strings.TrimPrefix(id, prefix), "%d", &n); err == nil && strings.HasPrefix(id, prefix) && n > max {
			max = n
		}
	}
	return fmt.Sprintf("%s%d", prefix, max+1), nil
}

// AsyncQuestionText extracts the question text of an async interaction.
func AsyncQuestionText(in *store.Interaction) string {
	if q, ok := in.Questions[delegate.AskUserQuestionKey].(string); ok {
		return q
	}
	return "(question unavailable)"
}

// AsyncAnswerText flattens the recorded answers of an answered async
// interaction. The canonical shape is a single AskUserQuestionKey string;
// other keys are rendered k=v so nothing is silently dropped.
func AsyncAnswerText(in *store.Interaction) string {
	if a, ok := in.Answers[delegate.AskUserQuestionKey].(string); ok && len(in.Answers) == 1 {
		return a
	}
	if len(in.Answers) == 0 {
		return "(empty answer)"
	}
	keys := make([]string, 0, len(in.Answers))
	for k := range in.Answers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, in.Answers[k]))
	}
	return strings.Join(parts, ", ")
}

// FormatAsyncAnswerMessage renders the operator-message text delivered to
// the asking node's inbox when its async question is answered. One
// canonical formatter so claw mid-turn injection, claude_code hook drains,
// and the resume fold-in all show the same shape.
func FormatAsyncAnswerMessage(interactionID, question, answer string) string {
	return fmt.Sprintf("[Answer to question %s] Q: %q — A: %s", interactionID, question, answer)
}