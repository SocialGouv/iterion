package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Async human interaction (ADR-081) — CLI surface for the local store.
// Cross-process by design: the answer is written to the run's store and
// queued node-scoped; a live engine in another process picks it up via
// the store-backed inbox drain (claw turn boundaries / claude_code
// hooks) and the await node's poll ticker.

// QuestionsOptions configures RunQuestions.
type QuestionsOptions struct {
	StoreDir string
	RunID    string
}

// RunQuestions lists a run's pending async questions.
func RunQuestions(opts QuestionsOptions, p *Printer) error {
	s, err := openLocalStore(opts.StoreDir)
	if err != nil {
		return err
	}
	ctx := context.Background()
	pending, err := store.ListPendingAsyncInteractions(ctx, s, opts.RunID, "")
	if err != nil {
		return err
	}
	if p.Format == OutputJSON {
		p.JSON(map[string]any{"interactions": pending})
		return nil
	}
	if len(pending) == 0 {
		p.Line("no pending async questions for run %s", opts.RunID)
		return nil
	}
	for _, in := range pending {
		p.Line("%s  (node %s, asked %s)", in.ID, in.NodeID, in.RequestedAt.Format("15:04:05"))
		p.Line("  Q: %s", model.AsyncQuestionText(in))
	}
	return nil
}

// AnswerOptions configures RunAnswer.
type AnswerOptions struct {
	StoreDir      string
	RunID         string
	InteractionID string
	Answer        string
}

// RunAnswer answers one pending async question: records the answer on
// the interaction, emits interaction_answered, and queues the
// node-scoped delivery message for the asking node.
func RunAnswer(opts AnswerOptions, p *Printer) error {
	if opts.Answer == "" {
		return UserInputError(fmt.Errorf("an answer text is required"))
	}
	s, err := openLocalStore(opts.StoreDir)
	if err != nil {
		return err
	}
	ctx := context.Background()
	in, err := s.LoadInteraction(ctx, opts.RunID, opts.InteractionID)
	if err != nil {
		return fmt.Errorf("interaction %s not found in run %s: %w (list them with `iterion runs questions %s`)", opts.InteractionID, opts.RunID, err, opts.RunID)
	}
	if in.Kind != store.InteractionKindAsync {
		return UserInputError(fmt.Errorf("interaction %s is not an async question (kind %q) — a blocking pause is answered with `iterion resume --answer`", opts.InteractionID, in.Kind))
	}
	answered, err := store.AnswerInteraction(ctx, s, opts.RunID, opts.InteractionID, map[string]any{delegate.AskUserQuestionKey: opts.Answer})
	if err != nil {
		return err
	}
	if evt, aerr := s.AppendEvent(ctx, opts.RunID, store.Event{
		Type:   store.EventInteractionAnswered,
		RunID:  opts.RunID,
		NodeID: answered.NodeID,
		Data:   map[string]any{"interaction_id": answered.ID, "async": true, "answer": opts.Answer},
	}); aerr != nil || evt == nil {
		p.Line("warning: answer recorded but event append failed: %v", aerr)
	}
	text := model.FormatAsyncAnswerMessage(answered.ID, model.AsyncQuestionText(answered), opts.Answer)
	msg := store.QueuedUserMessage{
		ID:     fmt.Sprintf("msg_%d_%s", time.Now().UnixNano(), answered.ID),
		RunID:  opts.RunID,
		Text:   text,
		NodeID: answered.NodeID,
	}
	if qerr := s.AppendQueuedMessage(ctx, opts.RunID, msg); qerr != nil {
		return fmt.Errorf("answer recorded but queueing delivery failed: %w", qerr)
	}
	if p.Format == OutputJSON {
		p.JSON(map[string]any{"run_id": opts.RunID, "interaction_id": answered.ID, "queued": true})
		return nil
	}
	p.Line("answered %s — delivery queued for node %s", answered.ID, answered.NodeID)
	return nil
}

func openLocalStore(storeDir string) (*store.FilesystemRunStore, error) {
	cwd, _ := os.Getwd()
	s, err := store.New(store.ResolveStoreDir(cwd, storeDir))
	if err != nil {
		return nil, fmt.Errorf("cannot open store: %w", err)
	}
	return s, nil
}
