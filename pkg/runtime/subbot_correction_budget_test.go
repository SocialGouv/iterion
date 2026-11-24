package runtime

import (
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"reflect"
	"time"

	"context"
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func TestFreshSubbotRetainsCorrectionAllowance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	parent := validationWorkflow()
	parent.Nodes["my_agent"] = &ir.SubbotNode{BaseNode: ir.BaseNode{ID: "my_agent"}, Source: "child.bot"}
	child := validationWorkflow()
	child.Nodes["my_agent"].(*ir.AgentNode).Backend = delegate.BackendClaudeCode
	child.Schemas["MySchema"].Fields[0].EnumValues = []string{"pass"}
	st := tmpStore(t)
	calls, children := 0, 0
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, adverseBackendFunc(func(context.Context, delegate.Task) (delegate.Result, error) {
		calls++
		return delegate.Result{Output: map[string]any{"summary": fmt.Sprint("wrong-", calls), "score": 1}, Tokens: 10}, nil
	}))
	childWorkDir := t.TempDir()
	runner := func(ctx context.Context, req SubbotRequest) (map[string]any, error) {
		children++
		id := fmt.Sprint("child-", children)
		if err := st.SetSubbotChild(ctx, req.ParentRunID, req.ReattachKey, id); err != nil {
			return nil, err
		}
		exec := model.NewClawExecutor(model.NewRegistry(), child, model.WithBackendRegistry(reg))
		eng := New(child, st, exec, WithWorkDir(childWorkDir), WithParentRunID(req.ParentRunID), WithParentNodeID(req.NodeID), WithOutputCorrectionBudget(1))
		prepared, err := prepareResourceInvocation(ctx, st, id, req.ParentRunID)
		if err != nil {
			return nil, err
		}
		return nil, eng.RunPrepared(prepared.Context, id, nil, prepared)
	}
	eng := New(parent, st, newStubExecutor(), WithSubbotRunner(runner), WithWorkDir(t.TempDir()))
	ctx := context.Background()
	if err := eng.Run(ctx, "parent", nil); err == nil {
		t.Fatal("invalid child succeeded")
	} else {
		t.Logf("first child: %v", err)
	}
	if calls != 2 {
		t.Fatalf("first child calls=%d", calls)
	}
	if err := eng.Resume(ctx, "parent", nil); err == nil {
		t.Fatal("invalid resumed child succeeded")
	} else {
		t.Logf("resumed child: %v", err)
	}
	if calls != 3 {
		t.Fatalf("parent resume resets child allowance: calls=%d want 3", calls)
	}
	run, err := st.LoadRun(ctx, "child-2")
	if err != nil {
		t.Fatal(err)
	}
	if ep := run.OutputCorrections["my_agent"]; ep.Attempts != 1 {
		t.Fatalf("seeded ledger=%+v", ep)
	}
}

func seededPaidEpisode() store.OutputCorrectionEpisode {
	at := time.Unix(100, 0).UTC()
	return store.OutputCorrectionEpisode{Generation: "old-generation", Revision: 7, LedgerKey: "old-ledger", EpisodeID: "episode", InvocationID: "node=node", NodeID: "node", Budget: 1, Attempts: 1, Status: correctionStatusExhausted, TerminalKind: "candidate_rejected", InputFingerprint: "input", LastOutputFingerprint: "output", LastViolationFingerprint: "violation", LastError: "rejected", StartedAt: at, UpdatedAt: at, RetiredAt: &at, RetiredReason: "old", Usage: store.OutputCorrectionUsage{Tokens: 13, CostUSD: 2}, Reservations: []store.OutputCorrectionReservation{{ID: "round", InitialUsageID: "initial", ExecutionEpoch: "old-epoch", ScopeKey: "old-run", Receipt: &store.OutputCorrectionReceipt{Usage: store.OutputCorrectionUsage{Tokens: 13, CostUSD: 2}}}}}
}
func TestSubbotCorrectionSeedClearsReceiptsPreservesPolicy(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			s, x := correctionFixture(t)
			ep := seededPaidEpisode()
			original := ep
			run := readCorrection(t, s)
			if existing {
				mine := store.OutputCorrectionEpisode{EpisodeID: "mine", Budget: 9, Attempts: 8, Status: "active"}
				if err := s.SetRunOutputCorrection(t.Context(), run.ID, "node", mine); err != nil {
					t.Fatal(err)
				}
				run = readCorrection(t, s)
			}
			e := New(validationWorkflow(), s, newStubExecutor(), WithParentRunID("parent"), WithParentNodeID("subbot"))
			seed := subbotCorrectionSeed{parentID: "parent", nodeID: "subbot", episodes: map[string]store.OutputCorrectionEpisode{"node": ep}}
			ctx := context.WithValue(t.Context(), subbotCorrectionKey{}, seed)
			if err := e.seedSubbotCorrections(ctx, run); err != nil {
				t.Fatal(err)
			}
			got := readCorrection(t, s).OutputCorrections["node"]
			if existing {
				if got.EpisodeID != "mine" || got.Attempts != 8 {
					t.Fatalf("existing replaced %+v", got)
				}
				return
			}
			if got.Generation != "" || got.Revision != 0 || got.LedgerKey != "" || len(got.Reservations) != 0 || got.Usage != (store.OutputCorrectionUsage{}) || got.RetiredAt != nil || got.RetiredReason != "" {
				t.Fatalf("paid/authority state transferred: %+v", got)
			}
			if got.Budget != ep.Budget || got.Attempts != ep.Attempts || got.Status != ep.Status || got.TerminalKind != ep.TerminalKind || got.InvocationID != ep.InvocationID || got.InputFingerprint != ep.InputFingerprint || got.LastOutputFingerprint != ep.LastOutputFingerprint || got.LastViolationFingerprint != ep.LastViolationFingerprint {
				t.Fatalf("policy lost %+v", got)
			}
			if !reflect.DeepEqual(seed.episodes["node"], original) {
				t.Fatal("mutated seed source")
			}
			a, _ := newCorrectionAccounts(&SharedBudget{}, nil)
			if err := a.reconcile(readCorrection(t, s)); err != nil {
				t.Fatalf("seed bill: %v", err)
			}
			if a.budget.tokensUsed != 0 || a.budget.costUsed != 0 {
				t.Fatal("prior child rebilled")
			}
			_ = x
		})
	}
}
func TestSubbotCorrectionSeedStrictRaiseAndCause(t *testing.T) {
	for _, kind := range []string{"equal", "raise", "unknown-terminal", "cause"} {
		t.Run(kind, func(t *testing.T) {
			s, x := correctionFixture(t)
			ep := seededPaidEpisode()
			if kind == "unknown-terminal" {
				ep.TerminalKind = ""
			}
			if kind == "cause" {
				ep.Cause = &store.OutputCorrectionCause{Version: 1, Kind: "auth", Provider: "provider"}
			}
			e := New(validationWorkflow(), s, newStubExecutor(), WithParentRunID("parent"), WithParentNodeID("subbot"))
			ctx := context.WithValue(t.Context(), subbotCorrectionKey{}, subbotCorrectionSeed{parentID: "parent", nodeID: "subbot", episodes: map[string]store.OutputCorrectionEpisode{"node": ep}})
			if err := e.seedSubbotCorrections(ctx, readCorrection(t, s)); err != nil {
				t.Fatal(err)
			}
			q := correctionReq()
			q.Budget = 2
			if kind == "equal" {
				q.Budget = 1
			}
			calls := 0
			q.Round = func(context.Context, map[string]any, error) (map[string]any, store.OutputCorrectionUsage, error) {
				calls++
				return map[string]any{"value": "valid"}, store.OutputCorrectionUsage{Tokens: 1}, nil
			}
			_, err := x.correct(t.Context(), q)
			want := 0
			if kind == "raise" {
				want = 1
			}
			if calls != want {
				t.Fatalf("%s calls=%d want%d err=%v", kind, calls, want, err)
			}
			if kind == "raise" && err != nil {
				t.Fatal(err)
			}
			if kind != "raise" && err == nil {
				t.Fatal("terminal/cause lost")
			}
			got := readCorrection(t, s).OutputCorrections["node"]
			if kind == "raise" && (got.Generation == "" || got.Generation == ep.Generation || got.Attempts != 2 || len(got.Reservations) != 1) {
				t.Fatalf("fresh authority/allowance %+v", got)
			}
			if kind == "cause" {
				var auth *delegate.ErrAuthFailed
				if !errors.As(err, &auth) || auth.Provider != "provider" {
					t.Fatalf("typed cause lost: %v", err)
				}
				if !reflect.DeepEqual(got.Cause, ep.Cause) {
					t.Fatal("cause changed")
				}
			}
		})
	}
}
func TestSubbotCorrectionContextSeedRejectsOtherParent(t *testing.T) {
	s, _ := correctionFixture(t)
	e := New(validationWorkflow(), s, newStubExecutor(), WithParentRunID("parent"), WithParentNodeID("subbot"))
	for _, seed := range []subbotCorrectionSeed{{parentID: "other", nodeID: "subbot", episodes: map[string]store.OutputCorrectionEpisode{"node": seededPaidEpisode()}}, {parentID: "parent", nodeID: "other", episodes: map[string]store.OutputCorrectionEpisode{"node": seededPaidEpisode()}}} {
		ctx := context.WithValue(t.Context(), subbotCorrectionKey{}, seed)
		if err := e.seedSubbotCorrections(ctx, readCorrection(t, s)); err != nil {
			t.Fatal(err)
		}
		if len(readCorrection(t, s).OutputCorrections) != 0 {
			t.Fatal("foreign seed applied")
		}
	}
}

func TestSubbotCorrectionSeedFromPriorChildLeavesPaidHistory(t *testing.T) {
	s := tmpStore(t)
	ctx := t.Context()
	if _, err := s.CreateRun(ctx, "parent", "wf", nil); err != nil {
		t.Fatal(err)
	}
	old, err := store.AsParentedRunCreator(s).CreateChildRun(ctx, "old-child", "wf", "parent", nil)
	if err != nil {
		t.Fatal(err)
	}
	old.Status = store.RunStatusFailedResumable
	ep := seededPaidEpisode()
	old.OutputCorrections = map[string]store.OutputCorrectionEpisode{"node": ep}
	old.InitialUsageReservations = map[string]store.OutputCorrectionReservation{"initial": {ID: "initial", InitialUsageID: "initial", ExecutionEpoch: "old-epoch", ScopeKey: "old-run", Receipt: &store.OutputCorrectionReceipt{Usage: store.OutputCorrectionUsage{Tokens: 7}, Observation: &store.UsageObservation{Version: 1, Tokens: 7, UnpricedTokens: 7, CountIteration: true}}}}
	if err = s.SaveRun(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err = s.SetSubbotChild(ctx, "parent", "attachment", "old-child"); err != nil {
		t.Fatal(err)
	}
	child, err := store.AsParentedRunCreator(s).CreateChildRun(ctx, "new-child", "wf", "parent", nil)
	if err != nil {
		t.Fatal(err)
	}
	oldBefore, err := s.LoadRun(ctx, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	parentBefore, err := s.LoadRun(ctx, "parent")
	if err != nil {
		t.Fatal(err)
	}
	parentEngine := New(validationWorkflow(), s, newStubExecutor())
	seeded, err := parentEngine.subbotCorrectionContext(ctx, &runState{runID: "parent"}, "subbot", "attachment")
	if err != nil {
		t.Fatal(err)
	}
	seeded, err = store.WithRunValueFormat(seeded, store.RunValueFormat{RunID: child.ID, Format: store.LegacyValueFormat})
	if err != nil {
		t.Fatal(err)
	}
	childEngine := New(validationWorkflow(), s, newStubExecutor(), WithParentRunID("parent"), WithParentNodeID("subbot"))
	if err = childEngine.seedSubbotCorrections(seeded, child); err != nil {
		t.Fatal(err)
	}
	oldAfter, err := s.LoadRun(ctx, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	parentAfter, err := s.LoadRun(ctx, "parent")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(oldBefore, oldAfter) || !reflect.DeepEqual(parentBefore, parentAfter) {
		t.Fatal("seed changed original child or parent")
	}
	child, err = s.LoadRun(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(child.InitialUsageReservations) != 0 || len(child.OutputCorrectionHistory) != 0 {
		t.Fatal("paid child history copied")
	}
	accounts, _ := newCorrectionAccounts(&SharedBudget{}, nil)
	if err = accounts.reconcile(oldAfter); err != nil {
		t.Fatal(err)
	}
	if accounts.budget.tokensUsed != 20 {
		t.Fatalf("prior run paid=%d", accounts.budget.tokensUsed)
	}
	if err = accounts.reconcile(child); err != nil {
		t.Fatal(err)
	}
	if accounts.budget.tokensUsed != 20 {
		t.Fatal("new child rebilled old receipts")
	}
}
