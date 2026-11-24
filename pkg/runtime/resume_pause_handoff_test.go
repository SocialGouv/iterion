package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

var errHandoffAnswerWrite = errors.New("injected interaction write error")

type handoffAnswerFaultStore struct {
	codemodeStateWrappedStore
	fault string
}

func (s handoffAnswerFaultStore) WriteInteraction(ctx context.Context, interaction *store.Interaction) error {
	if s.fault == "finish" {
		return s.RunStore.WriteInteraction(ctx, interaction)
	}
	if s.fault == "after write" {
		if err := s.RunStore.WriteInteraction(ctx, interaction); err != nil {
			return err
		}
	}
	return errHandoffAnswerWrite
}

func (s handoffAnswerFaultStore) FinishResumePauseHandoff(ctx context.Context, runID, handoffID string, cp *store.Checkpoint) (bool, error) {
	if s.fault == "finish" {
		return false, errHandoffAnswerWrite
	}
	return store.AsResumePauseHandoffStore(s.RunStore).FinishResumePauseHandoff(ctx, runID, handoffID, cp)
}

func (s handoffAnswerFaultStore) ReplaceResumePauseHandoff(ctx context.Context, runID, handoffID string, cp *store.Checkpoint) (bool, error) {
	return store.AsResumePauseHandoffStore(s.RunStore).ReplaceResumePauseHandoff(ctx, runID, handoffID, cp)
}

func (s handoffAnswerFaultStore) ParkResumePauseHandoff(ctx context.Context, runID, handoffID, reason string, budget *store.ResumePauseBudget) (bool, error) {
	return store.AsResumePauseHandoffStore(s.RunStore).ParkResumePauseHandoff(ctx, runID, handoffID, reason, budget)
}

func (s handoffAnswerFaultStore) ReclaimResumePauseHandoff(ctx context.Context, runID, oldID, newID string, status store.RunStatus, version int64, queuedAt time.Time) (bool, error) {
	return store.AsResumePauseHandoffStore(s.RunStore).ReclaimResumePauseHandoff(ctx, runID, oldID, newID, status, version, queuedAt)
}

func TestResumePauseHandoffRecoversAnswerWrite(t *testing.T) {
	for _, kind := range []string{"ordinary", "recovery"} {
		for _, fault := range []string{"before write", "after write", "finish"} {
			t.Run(kind+"/"+fault, func(t *testing.T) {
				ctx := t.Context()
				base := tmpStore(t)
				const id = "quota-handoff"
				engine := cfGateEngine
				answers := map[string]any{"decision": "go"}
				if kind == "recovery" {
					x := &flakyExecutor{target: "agent_a", failErr: &RuntimeError{Code: ErrCodeExecutionFailed, Message: "injected execution failure"}, failures: 1}
					engine = func(s store.RunStore, opts ...EngineOption) *Engine {
						return New(newRecoveryGateWorkflow(), s, x, append([]EngineOption{WithRecoveryDispatch(pauseForHumanOn(ErrCodeExecutionFailed))}, opts...)...)
					}
					answers = map[string]any{"acknowledge_recovery": "retry"}
				}
				if err := engine(base).Run(ctx, id, nil); !errors.Is(err, ErrRunPaused) {
					t.Fatalf("initial run did not pause: %v", err)
				}
				before, err := base.LoadRun(ctx, id)
				if err != nil || before.Checkpoint == nil || before.Checkpoint.InteractionID == "" {
					t.Fatalf("load initial pause: %v", err)
				}
				pointer := before.Checkpoint.InteractionID
				wrapped := handoffAnswerFaultStore{codemodeStateWrappedStore{base}, fault}
				if err := engine(wrapped, WithResumeCodemodeQuota(store.CodemodeScratchLimits{Entries: 7})).Resume(ctx, id, answers); !errors.Is(err, errHandoffAnswerWrite) {
					t.Fatalf("unexpected injected resume result: %v", err)
				}
				parked, err := base.LoadRun(ctx, id)
				if err != nil || parked.Status != store.RunStatusFailedResumable || parked.ResumePauseHandoff == nil || parked.ResumePauseHandoff.InteractionID != pointer {
					t.Fatalf("answer failure did not park the original handoff: %v", err)
				}
				if fault == "before write" {
					if err := engine(base).Resume(ctx, id, nil); err == nil {
						t.Fatal("resume crossed a pause whose answer never landed")
					}
				} else {
					answers = nil
				}
				if err := engine(base).Resume(ctx, id, answers); err != nil {
					t.Fatalf("retry: %v", err)
				}
				finished, err := base.LoadRun(ctx, id)
				if err != nil || finished.Status != store.RunStatusFinished || finished.ResumePauseHandoff != nil || finished.CodemodeBoundsOverride == nil || finished.CodemodeBoundsOverride.StoreEntries != 7 {
					t.Fatalf("retry did not finish with the committed quota: %v", err)
				}
				interaction, err := base.LoadInteraction(ctx, id, pointer)
				if err != nil || interaction.AnsweredAt == nil || len(interaction.Answers) == 0 {
					t.Fatalf("retry did not preserve the original answer: %v", err)
				}
			})
		}
	}
}
