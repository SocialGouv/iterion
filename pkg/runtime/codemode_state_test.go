package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/scripthost"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestCodemodeStateScopeAndResumeRestatement(t *testing.T) {
	ctx := store.WithTenant(t.Context(), "team-a")
	fs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"run-bank-a", "run-bank-b"} {
		if _, err := fs.CreateRun(ctx, id, "wf", nil); err != nil {
			t.Fatal(err)
		}
	}
	run, err := fs.LoadRun(ctx, "run-bank-a")
	if err != nil {
		t.Fatal(err)
	}
	run.Status = store.RunStatusFailedResumable
	run.CodemodeBoundsOverride = &scripthost.Limits{StoreEntries: 1, StoreBytes: 60, ToolCalls: 17}
	if err := fs.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	wf := &ir.Workflow{CodemodeBounds: &ir.CodemodeBounds{StoreEntries: 5}}
	e := &Engine{workflow: wf, store: codemodeStateWrappedStore{fs}}
	a := e.bindCodemodeState(ctx, run)
	if a == nil {
		t.Fatal("wrapping the store hid its state capability")
	}
	if err := a.StoreState(ctx, "k", json.RawMessage(`null`)); err != nil {
		t.Fatal(err)
	}
	if err := a.StoreState(ctx, "extra", json.RawMessage(`1`)); !errors.Is(err, scripthost.ErrStateQuota) {
		t.Fatalf("initial bank quota: %v", err)
	}

	run2, err := fs.LoadRun(ctx, "run-bank-b")
	if err != nil {
		t.Fatal(err)
	}
	b := e.bindCodemodeState(ctx, run2)
	if _, found, err := b.LoadState(ctx, "k"); found || err != nil {
		t.Fatalf("another run saw a value: found=%v err=%v", found, err)
	}
	if _, found, err := a.LoadState(store.WithTenant(ctx, "team-b"), "k"); found || !errors.Is(err, scripthost.ErrStateUnavailable) {
		t.Fatalf("another tenant's context did not refuse: found=%v err=%v", found, err)
	}

	e.resumeCodemodeQuota = store.CodemodeScratchLimits{Entries: 2}
	e.codemodeBoundsOverride = &scripthost.Limits{StoreEntries: 3, StoreBytes: 999, ToolCalls: 19}
	if claimed, err := e.casResumeClaim(ctx, run, []store.RunStatus{run.Status}); err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	persisted, err := fs.LoadRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b := persisted.CodemodeBoundsOverride; b == nil || b.StoreEntries != 2 || b.StoreBytes != 60 || b.ToolCalls != 17 {
		t.Fatalf("durable restatement = %+v", b)
	}
	resumed := e.bindCodemodeState(ctx, run)
	if value, found, err := resumed.LoadState(ctx, "k"); !found || err != nil || string(value) != "null" {
		t.Fatalf("same-run resume lost stored null: found=%v err=%v", found, err)
	}
	if err := resumed.StoreState(ctx, "extra", json.RawMessage(`1`)); err != nil {
		t.Fatalf("resumed writer did not use the restated quota: %v", err)
	}
	if err := a.StoreState(ctx, "extra", json.RawMessage(`22`)); !errors.Is(err, scripthost.ErrStateQuota) {
		t.Fatalf("existing handle did not freeze its quota: %v", err)
	}
}

func TestCodemodeQuotaPatchIsNotReplayedByTheSameEngine(t *testing.T) {
	ctx := t.Context()
	fs := tmpStore(t)
	const id = "quota-once"
	run, err := fs.CreateRun(ctx, id, "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{store: fs, resumeCodemodeQuota: store.CodemodeScratchLimits{Entries: 7},
		codemodeBoundsOverride: &scripthost.Limits{StoreEntries: 7, StoreBytes: 99}}
	run.Status = store.RunStatusFailedResumable
	if err := fs.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	allowed := []store.RunStatus{store.RunStatusFailedResumable}
	if claimed, err := e.casResumeClaim(ctx, run, allowed); !claimed || err != nil {
		t.Fatalf("first claim: claimed=%v, err=%v", claimed, err)
	}
	fresh, err := fs.LoadRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	fresh.Status = store.RunStatusFailedResumable
	fresh.CodemodeBoundsOverride = &scripthost.Limits{StoreEntries: 9, StoreBytes: 60}
	if err := fs.SaveRun(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if claimed, err := e.casResumeClaim(ctx, fresh, allowed); !claimed || err != nil {
		t.Fatalf("second claim: claimed=%v, err=%v", claimed, err)
	}
	persisted, err := fs.LoadRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.CodemodeBoundsOverride; got == nil || *got != (scripthost.Limits{StoreEntries: 9, StoreBytes: 60}) {
		t.Fatalf("later resume replayed a spent quota patch: %+v", got)
	}
}

func TestCodemodeStateSurvivesBranchConstruction(t *testing.T) {
	parent := &runState{runID: "run-bank", codemodeState: runCodemodeState{
		runID: "run-bank", limits: store.CodemodeScratchLimits{Entries: 13, Bytes: 2048},
	}}
	parallel := &parallelExecutionState{}
	parallel.captureBase(parent)
	base := parallel.branchBase(parent)
	for name, branch := range map[string]*runState{
		"captured base":   base,
		"direct branch":   newBranchRunState(parent, &store.BranchCheckpoint{}, &branchResult{}),
		"captured branch": newBranchRunState(base, &store.BranchCheckpoint{}, &branchResult{}),
	} {
		t.Run(name, func(t *testing.T) {
			if branch.codemodeState != parent.codemodeState {
				t.Fatalf("branch bank = %#v, want the parent run's bank and quotas", branch.codemodeState)
			}
		})
	}
}

type codemodeStateWrappedStore struct{ store.RunStore }

func (w codemodeStateWrappedStore) Unwrap() store.RunStore { return w.RunStore }

var _ model.CodemodeState = runCodemodeState{}

// quotaClaimFaultStore fails the quota-bearing write, wherever the runtime
// attempts it. The legacy whole-document write stays broken even on retry;
// the atomic claim can recover independently.
type quotaClaimFaultStore struct {
	codemodeStateWrappedStore
	fail bool
}

var errQuotaClaimWrite = errors.New("injected quota write failure")

func (s *quotaClaimFaultStore) SaveRun(ctx context.Context, r *store.Run) error {
	if r.CodemodeBoundsOverride != nil && r.CodemodeBoundsOverride.StoreEntries == 7 {
		return errQuotaClaimWrite
	}
	return s.RunStore.SaveRun(ctx, r)
}

func (s *quotaClaimFaultStore) ClaimResumeWithCodemodeQuota(ctx context.Context, id string, claim store.CodemodeQuotaResumeClaim, quota store.CodemodeScratchLimits) (*scripthost.Limits, error) {
	if s.fail {
		return nil, errQuotaClaimWrite
	}
	return store.AsCodemodeQuotaResumeClaimer(s.RunStore).ClaimResumeWithCodemodeQuota(ctx, id, claim, quota)
}

func TestCodemodeQuotaClaimFailurePreservesPauseForRetry(t *testing.T) {
	for _, queued := range []bool{false, true} {
		for _, recovery := range []bool{false, true} {
			name := "ordinary"
			if recovery {
				name = "recovery"
			}
			if queued {
				name = "queued/" + name
			} else {
				name = "direct/" + name
			}
			t.Run(name, func(t *testing.T) {
				ctx := t.Context()
				base := tmpStore(t)
				const id = "quota-claim-failure"
				engine := cfGateEngine
				answers := map[string]any{"decision": "go"}
				if recovery {
					x := &flakyExecutor{target: "agent_a", failErr: &RuntimeError{Code: ErrCodeExecutionFailed, Message: "injected executor failure"}, failures: 1}
					engine = func(s store.RunStore, opts ...EngineOption) *Engine {
						return New(newRecoveryGateWorkflow(), s, x, append([]EngineOption{WithRecoveryDispatch(pauseForHumanOn(ErrCodeExecutionFailed))}, opts...)...)
					}
					answers = map[string]any{"acknowledge_recovery": "retry"}
				}
				if err := engine(base).Run(ctx, id, nil); !errors.Is(err, ErrRunPaused) {
					t.Fatalf("setup did not pause: %v", err)
				}
				var published time.Time
				if queued {
					published = queueAttempt(t, base, id, store.RunStatusPausedWaitingHuman)
				}
				before, err := base.LoadRun(ctx, id)
				if err != nil || before.Checkpoint == nil || before.Checkpoint.InteractionID == "" || before.Checkpoint.RecoveryPause != recovery {
					t.Fatalf("setup lost pause: run=%+v, err=%v", before, err)
				}
				pointer := before.Checkpoint.InteractionID
				var exactMarker time.Time
				if queued {
					exactMarker = *before.QueuedAt
				}
				before.CodemodeBoundsOverride = &scripthost.Limits{StoreEntries: 2, StoreBytes: 60, ToolCalls: 17}
				if err := base.SaveRun(ctx, before); err != nil {
					t.Fatal(err)
				}
				fault := &quotaClaimFaultStore{codemodeStateWrappedStore: codemodeStateWrappedStore{base}, fail: true}
				claimed := false
				resume := func() error {
					return engine(fault, WithQueuedAttempt(published), WithResumeCodemodeQueuedAt(exactMarker), WithCodemodeOverride("", &scripthost.Limits{StoreEntries: 8, StoreBytes: 999, ToolCalls: 99}), WithResumeCodemodeQuota(store.CodemodeScratchLimits{Entries: 7}), WithOnResumeClaimed(func() { claimed = true })).Resume(ctx, id, answers)
				}
				since := cfEventCount(t, base, id)
				if err := resume(); !errors.Is(err, errQuotaClaimWrite) {
					t.Fatalf("want the injected quota write failure, got %v", err)
				}
				after, err := base.LoadRun(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if after.Status != before.Status || after.Checkpoint == nil || after.Checkpoint.InteractionID != pointer || after.Checkpoint.RecoveryPause != recovery || claimed {
					t.Fatalf("failed quota write consumed the pause: status=%s, pointer and recovery marker preserved=%v, claimed=%v", after.Status, after.Checkpoint != nil && after.Checkpoint.InteractionID == pointer && after.Checkpoint.RecoveryPause == recovery, claimed)
				}
				if got := after.CodemodeBoundsOverride; got == nil || *got != *before.CodemodeBoundsOverride {
					t.Fatalf("failed claim changed quota: %+v", got)
				}
				if writes := cfWritesSince(t, base, id, since); len(writes) != 0 {
					t.Fatalf("failed claim wrote resume effects: %v", writes)
				}
				fault.fail = false
				if err := resume(); err != nil || !claimed {
					t.Fatalf("retry did not claim and finish: claimed=%v, err=%v", claimed, err)
				}
				finished, err := base.LoadRun(ctx, id)
				if err != nil || finished.Status != store.RunStatusFinished {
					t.Fatalf("retry did not finish: run=%+v, err=%v", finished, err)
				}
				if got := finished.CodemodeBoundsOverride; got == nil || *got != (scripthost.Limits{StoreEntries: 7, StoreBytes: 60, ToolCalls: 17}) {
					t.Fatalf("winning claim did not merge just the asked quota: %+v", got)
				}
				it, err := base.LoadInteraction(ctx, id, pointer)
				if err != nil || it.AnsweredAt == nil || len(it.Answers) == 0 {
					t.Fatalf("retry did not answer the original pause: interaction=%+v, err=%v", it, err)
				}
			})
		}
	}
}
