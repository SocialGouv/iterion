package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func correctionFixture(t *testing.T) (*store.FilesystemRunStore, *correctionExecution) {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRun(context.Background(), "correction", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	x, err := acquireCorrectionExecution(context.Background(), s, r, store.ExecutionStart, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return s, x
}
func correctionReq() correctionRequest {
	return correctionRequest{Key: "node", NodeID: "node", InvocationID: "node=node", ScopeKey: "correction", Budget: 3, Output: map[string]any{"value": 0}, ErrorOnRejection: errors.New("original producer failure"), Validate: func(out map[string]any) error {
		if out["value"] == "valid" {
			return nil
		}
		return fmt.Errorf("bad value %v", out["value"])
	}}
}
func readCorrection(t *testing.T, s store.RunStore) *store.Run {
	t.Helper()
	r, e := s.LoadRun(context.Background(), "correction")
	if e != nil {
		t.Fatal(e)
	}
	return r
}

func TestCorrectionCASCandidateProtocol(t *testing.T) {
	for _, tc := range []struct {
		name    string
		budget  int
		outputs []any
		calls   int
		status  string
	}{
		{"valid", 3, []any{"valid"}, 1, "succeeded"}, {"unchanged", 3, []any{0}, 1, "unchanged"}, {"exhausted", 3, []any{1, 2, 3}, 3, "exhausted"}, {"zero", 0, nil, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, x := correctionFixture(t)
			q := correctionReq()
			q.Budget = tc.budget
			calls, finalized := 0, 0
			q.Round = func(ctx context.Context, _ map[string]any, _ error) (map[string]any, store.OutputCorrectionUsage, error) {
				r := readCorrection(t, s)
				ep := r.OutputCorrections[q.Key]
				if ep.Attempts != calls+1 || len(ep.Reservations) != calls+1 {
					t.Fatal("paid call was not reserved")
				}
				v := tc.outputs[calls]
				calls++
				return map[string]any{"value": v, "_tokens": 10 + calls}, store.OutputCorrectionUsage{Tokens: 1, CostUSD: 1}, nil
			}
			q.Finalize = func(context.Context, map[string]any) error { finalized++; return nil }
			out, err := x.correct(context.Background(), q)
			ep := readCorrection(t, s).OutputCorrections[q.Key]
			if calls != tc.calls || ep.Status != tc.status || ep.Attempts != tc.calls || ep.Usage.Tokens != tc.calls || ep.Usage.CostUSD != float64(tc.calls) {
				t.Fatalf("calls=%d episode=%+v", calls, ep)
			}
			if tc.status == "succeeded" {
				if err != nil || finalized != 1 || out["_tokens"] != 11 {
					t.Fatalf("success %v %v finalize=%d", out, err, finalized)
				}
			} else if !errors.Is(err, q.ErrorOnRejection) || finalized != 0 {
				t.Fatalf("original cause lost: %v finalized=%d", err, finalized)
			}
		})
	}
}
func TestCorrectionCASBudgetRaiseOnlyRejectedCandidates(t *testing.T) {
	for _, roundFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(roundFailure), func(t *testing.T) {
			s, x := correctionFixture(t)
			q := correctionReq()
			q.Budget = 1
			calls := 0
			q.Round = func(context.Context, map[string]any, error) (map[string]any, store.OutputCorrectionUsage, error) {
				calls++
				if roundFailure {
					return nil, store.OutputCorrectionUsage{}, errors.New("provider refused")
				}
				return map[string]any{"value": calls}, store.OutputCorrectionUsage{}, nil
			}
			_, _ = x.correct(context.Background(), q)
			q.Budget = 2
			_, _ = x.correct(context.Background(), q)
			want := 2
			if roundFailure {
				want = 1
			}
			if calls != want {
				t.Fatalf("calls %d want %d", calls, want)
			}
			if readCorrection(t, s).OutputCorrections[q.Key].Attempts != want {
				t.Fatal("attempt ledger differs")
			}
		})
	}
}
func TestCorrectionCASStaleBeforeAndAfterPaidCall(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprint(after), func(t *testing.T) {
			s, x := correctionFixture(t)
			q := correctionReq()
			calls := 0
			stop := func() {
				if err := s.UpdateRunStatus(context.Background(), "correction", store.RunStatusPausedWaitingHuman, ""); err != nil {
					t.Fatal(err)
				}
			}
			if !after {
				stop()
			}
			q.Round = func(context.Context, map[string]any, error) (map[string]any, store.OutputCorrectionUsage, error) {
				calls++
				stop()
				r := readCorrection(t, s)
				ep := r.OutputCorrections[q.Key]
				ep.LedgerKey = q.Key
				r.OutputCorrectionHistory = append(r.OutputCorrectionHistory, ep)
				delete(r.OutputCorrections, q.Key)
				if err := s.SaveRun(context.Background(), r); err != nil {
					t.Fatal(err)
				}
				_, err := acquireCorrectionExecution(context.Background(), s, r, store.ExecutionResume, time.Time{})
				if err != nil {
					t.Fatal(err)
				}
				return map[string]any{"value": "valid"}, store.OutputCorrectionUsage{Tokens: 7, CostUSD: 2}, nil
			}
			_, err := x.correct(context.Background(), q)
			if !errors.Is(err, errCorrectionOwnershipLost) {
				t.Fatalf("not stopped: %v", err)
			}
			want := 0
			if after {
				want = 1
			}
			if calls != want {
				t.Fatalf("calls=%d", calls)
			}
			if after {
				r := readCorrection(t, s)
				ep := r.OutputCorrectionHistory[0]
				if ep.Status != "active" || ep.Attempts != 1 || ep.Usage.Tokens != 7 || ep.Usage.CostUSD != 2 {
					t.Fatalf("receipt changed verdict or lost bill: %+v", ep)
				}
			}
		})
	}
}
func TestCorrectionCASCancelStillSettlesAndAnsweredWins(t *testing.T) {
	for _, answered := range []bool{false, true} {
		t.Run(fmt.Sprint(answered), func(t *testing.T) {
			s, x := correctionFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			q := correctionReq()
			q.Round = func(context.Context, map[string]any, error) (map[string]any, store.OutputCorrectionUsage, error) {
				cancel()
				if answered {
					return map[string]any{"value": "valid"}, store.OutputCorrectionUsage{Tokens: 5}, nil
				}
				return nil, store.OutputCorrectionUsage{Tokens: 5}, context.Canceled
			}
			_, err := x.correct(ctx, q)
			ep := readCorrection(t, s).OutputCorrections[q.Key]
			if ep.Usage.Tokens != 5 {
				t.Fatal("cancel lost receipt")
			}
			if answered {
				if err != nil || ep.Status != "succeeded" {
					t.Fatalf("arrived answer lost %v %+v", err, ep)
				}
			} else if !errors.Is(err, context.Canceled) || ep.Status != "active" {
				t.Fatalf("interruption %v %+v", err, ep)
			}
		})
	}
}
func TestCorrectionCauseWirePriority(t *testing.T) {
	cases := []error{&delegate.ErrAuthFailed{Provider: "claude_code", Detail: "SECRET"}, &delegate.ErrRateLimited{Provider: "claw", Kind: delegate.RateLimitKindUsageWindow, ResetAt: time.Unix(100, 0), SelfImposed: true}, correctionAmbiguousCause{}}
	for _, err := range cases {
		c := correctionCause(errors.Join(errors.New("ordinary"), err))
		if c == nil {
			t.Fatal("no cause")
		}
		for _, codec := range []string{"json", "bson"} {
			var out store.OutputCorrectionCause
			var b []byte
			var e error
			if codec == "json" {
				b, e = json.Marshal(c)
				if e == nil {
					e = json.Unmarshal(b, &out)
				}
			} else {
				b, e = bson.Marshal(c)
				if e == nil {
					e = bson.Unmarshal(b, &out)
				}
			}
			if e != nil {
				t.Fatal(e)
			}
			got := rehydrateCorrectionCause(&out)
			if correctionCause(got) == nil || correctionCause(got).Kind != c.Kind {
				t.Fatalf("%s lost type %v", codec, got)
			}
		}
	}
	joined := errors.Join(cases[0], cases[1], cases[2])
	if c := correctionCause(joined); c.Kind != "ambiguous" || !errtrack.IsAmbiguousEffect(rehydrateCorrectionCause(c)) {
		t.Fatalf("priority=%+v", c)
	}
	if rehydrateCorrectionCause(&store.OutputCorrectionCause{Version: 99, Kind: "auth"}) == nil {
		t.Fatal("unknown wire accepted")
	}
}
func TestCorrectionAccountsCrashReopenHistoryAndDaily(t *testing.T) {
	s, x := correctionFixture(t)
	q := correctionReq()
	q.ScopeKey = "branch.scope"
	q.Round = func(context.Context, map[string]any, error) (map[string]any, store.OutputCorrectionUsage, error) {
		return map[string]any{"value": "valid"}, store.OutputCorrectionUsage{Tokens: 5, CostUSD: 1, UnreportedCalls: 2}, nil
	}
	_, err := x.correct(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	// Crash before any booking. Read the durable FS ledger through a new handle.
	reopened, err := store.New(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	r := readCorrection(t, reopened)
	b := newSharedBudget(&ir.Budget{MaxTokens: 1000}, nil)
	b.Restore(0, 3, 0, 0, 0, 0, 0)
	a, _ := newCorrectionAccounts(b, nil)
	if err := a.raiseDailyBases(map[string]float64{"branch.scope": 3}); err != nil {
		t.Fatal(err)
	}
	if err := a.reconcile(r); err != nil {
		t.Fatal(err)
	}
	if err := a.reconcile(r); err != nil {
		t.Fatal(err)
	}
	cp := &store.Checkpoint{}
	state := a.snapshot(cp)
	if cp.BudgetTokensUsed != 5 || cp.BudgetCostUSD != 4 || cp.BudgetUnreportedCalls != 2 || cp.BudgetIterationsUsed != 0 || state.DailyScopeCostUSD["branch.scope"] != 4 {
		t.Fatalf("cp=%+v state=%+v", cp, state)
	}
	// Rewind removes Parallel and moves current generation to history. The
	// external carrier and cumulative daily base survive unchanged.
	ep := r.OutputCorrections[q.Key]
	ep.LedgerKey = q.Key
	r.OutputCorrectionHistory = append(r.OutputCorrectionHistory, ep)
	r.OutputCorrections = nil
	for _, codec := range []string{"json", "bson"} {
		var restored store.CorrectionAccounting
		if codec == "json" {
			wire, e := json.Marshal(state)
			if e != nil {
				t.Fatal(e)
			}
			if e = json.Unmarshal(wire, &restored); e != nil {
				t.Fatal(e)
			}
		} else {
			wire, e := bson.Marshal(state)
			if e != nil {
				t.Fatal(e)
			}
			if e = bson.Unmarshal(wire, &restored); e != nil {
				t.Fatal(e)
			}
		}
		nextBudget := newSharedBudget(&ir.Budget{MaxTokens: 1000}, nil)
		nextBudget.Restore(cp.BudgetTokensUsed, cp.BudgetCostUSD, 0, 0, 0, 0, cp.BudgetUnreportedCalls)
		next, e := newCorrectionAccounts(nextBudget, &restored)
		if e != nil {
			t.Fatal(e)
		}
		if e = next.reconcile(r); e != nil {
			t.Fatal(e)
		}
		after := &store.Checkpoint{}
		saved := next.snapshot(after)
		if after.BudgetTokensUsed != 5 || after.BudgetCostUSD != 4 || saved.DailyScopeCostUSD["branch.scope"] != 4 {
			t.Fatalf("%s replay double booked: %+v %+v", codec, after, saved)
		}
	}
}
func TestCorrectionAccountsConcurrentSnapshot(t *testing.T) {
	b := newSharedBudget(&ir.Budget{MaxTokens: 100000}, nil)
	a, _ := newCorrectionAccounts(b, nil)
	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Go(func() {
			for n := 0; n < 20; n++ {
				if err := a.book("scope", store.OutputCorrectionUsage{Tokens: 1, CostUSD: 1}, true); err != nil {
					t.Error(err)
				}
				cp := &store.Checkpoint{}
				state := a.snapshot(cp)
				if float64(cp.BudgetTokensUsed) != state.DailyScopeCostUSD["scope"] {
					t.Errorf("torn snapshot tokens=%d daily=%v", cp.BudgetTokensUsed, state.DailyScopeCostUSD)
				}
			}
		})
	}
	wg.Wait()
	cp := &store.Checkpoint{}
	a.snapshot(cp)
	if cp.BudgetTokensUsed != 500 || cp.BudgetIterationsUsed != 500 {
		t.Fatalf("lost counts %+v", cp)
	}
}

type delayedCorrectionClaim struct {
	*store.FilesystemRunStore
	after func()
}

func (s *delayedCorrectionClaim) AcquireRunExecutionIf(ctx context.Context, id string, want store.RunExecutionExpectation, intent store.RunExecutionIntent) (store.RunExecutionAuthority, bool, error) {
	grant, ok, err := s.FilesystemRunStore.AcquireRunExecutionIf(ctx, id, want, intent)
	if ok && err == nil {
		s.after()
	}
	return grant, ok, err
}
func TestCorrectionAuthorityRetainsClaimResponse(t *testing.T) {
	s, _ := correctionFixture(t)
	r := readCorrection(t, s)
	var successor store.RunExecutionAuthority
	delayed := &delayedCorrectionClaim{FilesystemRunStore: s}
	delayed.after = func() {
		if err := s.UpdateRunStatus(context.Background(), r.ID, store.RunStatusPausedWaitingHuman, ""); err != nil {
			t.Fatal(err)
		}
		fresh := readCorrection(t, s)
		next, e := acquireCorrectionExecution(context.Background(), s, fresh, store.ExecutionResume, time.Time{})
		if e != nil {
			t.Fatal(e)
		}
		successor = next.authority
	}
	old, err := acquireCorrectionExecution(context.Background(), delayed, r, store.ExecutionStart, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if old.authority == successor {
		t.Fatal("delayed claim stole successor's authority")
	}
	q := correctionReq()
	q.Round = func(context.Context, map[string]any, error) (map[string]any, store.OutputCorrectionUsage, error) {
		t.Fatal("stale grant bought call")
		return nil, store.OutputCorrectionUsage{}, nil
	}
	_, err = old.correct(context.Background(), q)
	if !errors.Is(err, errCorrectionOwnershipLost) {
		t.Fatalf("stale response: %v", err)
	}
}
func TestCorrectionAccountsDailyMarksIndependentAndUnpriced(t *testing.T) {
	b := newSharedBudget(&ir.Budget{MaxTokens: 1000}, nil)
	a, _ := newCorrectionAccounts(b, nil)
	if err := a.book("branch", store.OutputCorrectionUsage{Tokens: 2, CostUSD: 3}, true); err != nil {
		t.Fatal(err)
	}
	r := &store.Run{OutputCorrections: map[string]store.OutputCorrectionEpisode{"k": {Generation: "g", Usage: store.OutputCorrectionUsage{Tokens: 7, CostUSD: 1, UnreportedCalls: 2}, Reservations: []store.OutputCorrectionReservation{
		{ScopeKey: "branch", Receipt: &store.OutputCorrectionReceipt{Usage: store.OutputCorrectionUsage{Tokens: 4}}},
		{ScopeKey: "branch", Receipt: &store.OutputCorrectionReceipt{Usage: store.OutputCorrectionUsage{Tokens: 3, CostUSD: 1, UnreportedCalls: 2}}},
	}}}}
	if err := a.reconcile(r); err != nil {
		t.Fatal(err)
	}
	cp := &store.Checkpoint{}
	saved := a.snapshot(cp)
	if cp.BudgetTokensUsed != 9 || cp.BudgetCostUSD != 4 || cp.BudgetIterationsUsed != 1 || cp.BudgetUnpricedTokens != 4 || cp.BudgetUnreportedCalls != 2 {
		t.Fatalf("wrong marginal axes %+v", cp)
	}
	// Budget mark exists while the daily mark does not. Its delta must still
	// be applied; reusing budget marks for daily would leave the cost at 3.
	saved.DailyMarks = nil
	saved.DailyScopeCostUSD = map[string]float64{"branch": 3}
	next, _ := newCorrectionAccounts(b, &saved)
	if err := next.reconcile(r); err != nil {
		t.Fatal(err)
	}
	after := &store.Checkpoint{}
	result := next.snapshot(after)
	if after.BudgetCostUSD != 4 || result.DailyScopeCostUSD["branch"] != 4 {
		t.Fatalf("daily/budget coupled: %+v %+v", after, result)
	}
	if err := next.book("branch", store.OutputCorrectionUsage{UnreportedCalls: 1}, false); err != nil {
		t.Fatal(err)
	}
	next.snapshot(after)
	if after.BudgetIterationsUsed != 1 || after.BudgetUnreportedCalls != 3 {
		t.Fatal("unreported-only added iteration or vanished")
	}
}
func TestCorrectionAccountsRealDailyGuardReplay(t *testing.T) {
	s, _ := correctionFixture(t)
	guard := NewDailyCapGuard(s, nil, DailyCapConfig{MaxCostPerDayUSD: 20})
	if _, err := guard.Record(context.Background(), "branch", 3); err != nil {
		t.Fatal(err)
	}
	b := newSharedBudget(&ir.Budget{MaxCostUSD: 20}, nil)
	b.Restore(0, 3, 0, 0, 0, 0, 0)
	a, _ := newCorrectionAccounts(b, nil)
	if err := a.restoreDaily(context.Background(), guard, map[string]float64{"branch": 1}); err != nil {
		t.Fatal(err)
	}
	r := &store.Run{OutputCorrectionHistory: []store.OutputCorrectionEpisode{{LedgerKey: "k", Generation: "old", Usage: store.OutputCorrectionUsage{CostUSD: 1}, Reservations: []store.OutputCorrectionReservation{{ScopeKey: "branch", Receipt: &store.OutputCorrectionReceipt{Usage: store.OutputCorrectionUsage{CostUSD: 1}}}}}}}
	for i := 0; i < 2; i++ {
		if err := a.reconcile(r); err != nil {
			t.Fatal(err)
		}
		if err := a.flushDaily(context.Background(), guard); err != nil {
			t.Fatal(err)
		}
	}
	cp := &store.Checkpoint{}
	a.snapshot(cp)
	status, err := guard.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cp.BudgetCostUSD != 4 || status.SpentUSD != 4 {
		t.Fatalf("rewind bill budget=%v daily=%v", cp.BudgetCostUSD, status.SpentUSD)
	}
}

func TestCorrectionCASDurablePrioritySurvivesBudgetZero(t *testing.T) {
	s, x := correctionFixture(t)
	q := correctionReq()
	calls := 0
	q.Round = func(context.Context, map[string]any, error) (map[string]any, store.OutputCorrectionUsage, error) {
		calls++
		return nil, store.OutputCorrectionUsage{Tokens: 2}, &delegate.ErrAuthFailed{Provider: "claw", Detail: "SECRET"}
	}
	_, err := x.correct(context.Background(), q)
	var auth *delegate.ErrAuthFailed
	if !errors.As(err, &auth) {
		t.Fatalf("live cause %v", err)
	}
	q.Budget = 0
	_, err = x.correct(context.Background(), q)
	if !errors.As(err, &auth) || calls != 1 {
		t.Fatalf("budget zero erased durable cause: %v calls=%d", err, calls)
	}
	ep := readCorrection(t, s).OutputCorrections[q.Key]
	if ep.LastError != "output correction stopped" || ep.TerminalKind != "round_failed" {
		t.Fatalf("durable diagnostics %+v", ep)
	}
}

type refusingCorrectionDecision struct {
	store.OutputCorrectionCASStore
	deny string
}

func (s refusingCorrectionDecision) SetRunOutputCorrectionIf(ctx context.Context, id string, a store.RunExecutionAuthority, key string, want store.OutputCorrectionExpected, next store.OutputCorrectionEpisode) (bool, error) {
	if s.deny == "create" && want.Absent || s.deny == "reserve" && next.Attempts > want.Attempts || s.deny == "verdict" && next.Status == "succeeded" {
		return false, nil
	}
	return s.OutputCorrectionCASStore.SetRunOutputCorrectionIf(ctx, id, a, key, want, next)
}
func TestCorrectionCASConflictNeverReplaysOrFinalizes(t *testing.T) {
	for _, phase := range []string{"create", "reserve", "verdict"} {
		t.Run(phase, func(t *testing.T) {
			s, x := correctionFixture(t)
			x.ledger = refusingCorrectionDecision{x.ledger, phase}
			q := correctionReq()
			calls, finalized := 0, 0
			q.Round = func(context.Context, map[string]any, error) (map[string]any, store.OutputCorrectionUsage, error) {
				calls++
				return map[string]any{"value": "valid"}, store.OutputCorrectionUsage{Tokens: 7}, nil
			}
			q.Finalize = func(context.Context, map[string]any) error { finalized++; return nil }
			_, err := x.correct(context.Background(), q)
			if !errors.Is(err, errCorrectionOwnershipLost) || finalized != 0 {
				t.Fatalf("conflict became permission: err=%v finalized=%d", err, finalized)
			}
			want := 0
			if phase == "verdict" {
				want = 1
				ep := readCorrection(t, s).OutputCorrections[q.Key]
				if ep.Usage.Tokens != 7 || ep.Status != "active" {
					t.Fatalf("paid receipt/verdict %+v", ep)
				}
			}
			if calls != want {
				t.Fatalf("conflict bought calls=%d want=%d", calls, want)
			}
		})
	}
}
