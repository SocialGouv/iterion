package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"sync"

	"github.com/SocialGouv/iterion/pkg/store"
)

// correctionAccounts serializes receipt reconciliation and checkpoint capture.
// Ordinary node booking must use book as well once this carrier is installed;
// otherwise an independent daily total could race a correction snapshot.
// Lock order is accounts.mu -> budget.mu. No caller holds the latter first.
type correctionAccounts struct {
	mu     sync.Mutex
	state  store.CorrectionAccounting
	budget *SharedBudget
}

func newCorrectionAccounts(b *SharedBudget, saved *store.CorrectionAccounting) (*correctionAccounts, error) {
	a := &correctionAccounts{budget: b, state: store.CorrectionAccounting{Version: 1}}
	if saved != nil {
		if saved.Version != 1 {
			return nil, errors.New("output correction: unsupported accounting version")
		}
		for _, u := range saved.UsageMarks {
			if !validCorrectionUsage(u) {
				return nil, errors.New("output correction: invalid saved usage mark")
			}
		}
		for _, n := range saved.UnpricedMarks {
			if n < 0 {
				return nil, errors.New("output correction: invalid saved unpriced mark")
			}
		}
		for _, marks := range []map[string]float64{saved.DailyMarks, saved.DailyScopeCostUSD} {
			for _, v := range marks {
				if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
					return nil, errors.New("output correction: invalid saved daily mark")
				}
			}
		}
		a.state = cloneCorrectionAccounts(*saved)
	}
	if a.state.IterationMarks == nil {
		a.state.IterationMarks = map[string]bool{}
	}
	if a.state.UnpricedNodeMarks == nil {
		a.state.UnpricedNodeMarks = map[string]bool{}
	}
	if a.state.UsageMarks == nil {
		a.state.UsageMarks = map[string]store.OutputCorrectionUsage{}
	}
	if a.state.UnpricedMarks == nil {
		a.state.UnpricedMarks = map[string]int{}
	}
	if a.state.DailyMarks == nil {
		a.state.DailyMarks = map[string]float64{}
	}
	if a.state.DailyScopeCostUSD == nil {
		a.state.DailyScopeCostUSD = map[string]float64{}
	}
	return a, nil
}
func cloneCorrectionAccounts(s store.CorrectionAccounting) store.CorrectionAccounting {
	s.IterationMarks = cloneMap(s.IterationMarks)
	s.UnpricedNodeMarks = cloneMap(s.UnpricedNodeMarks)
	s.UsageMarks = cloneMap(s.UsageMarks)
	s.UnpricedMarks = cloneMap(s.UnpricedMarks)
	s.DailyMarks = cloneMap(s.DailyMarks)
	s.DailyScopeCostUSD = cloneMap(s.DailyScopeCostUSD)
	return s
}
func correctionAccountingKey(parts ...string) string {
	h := sha256.New()
	var n [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		_, _ = h.Write(n[:])
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}
func addCorrectionCount(dst *int, n int) error {
	if n < 0 || n > math.MaxInt-*dst {
		return errors.New("output correction: accounting count overflow")
	}
	*dst += n
	return nil
}
func checkedCorrectionUsageSum(a, b store.OutputCorrectionUsage) (store.OutputCorrectionUsage, error) {
	if !validCorrectionUsage(a) || !validCorrectionUsage(b) || b.Tokens > math.MaxInt-a.Tokens || b.UnreportedCalls > math.MaxInt-a.UnreportedCalls || math.IsInf(a.CostUSD+b.CostUSD, 0) {
		return a, errors.New("output correction: accounting overflow")
	}
	return store.OutputCorrectionUsage{Tokens: a.Tokens + b.Tokens, CostUSD: a.CostUSD + b.CostUSD, UnreportedCalls: a.UnreportedCalls + b.UnreportedCalls}, nil
}
func checkCorrectionBudgetAddition(b *SharedBudget, u store.OutputCorrectionUsage, unpriced, nodes, iterations int) error {
	if _, err := checkedCorrectionUsageSum(store.OutputCorrectionUsage{Tokens: b.tokensUsed, CostUSD: b.costUsed, UnreportedCalls: b.unreportedCalls}, u); err != nil {
		return err
	}
	if unpriced < 0 || nodes < 0 || iterations < 0 || unpriced > math.MaxInt-b.unpricedTokens || nodes > math.MaxInt-b.unpricedNodes || iterations > math.MaxInt-b.iterationsUsed {
		return errors.New("output correction: accounting counter overflow")
	}
	return nil
}
func validCorrectionUsage(u store.OutputCorrectionUsage) bool {
	return u.Tokens >= 0 && u.CostUSD >= 0 && !math.IsNaN(u.CostUSD) && !math.IsInf(u.CostUSD, 0) && u.UnreportedCalls >= 0
}

// book accounts an ordinary execution once. The caller supplies the actual
// daily ledger scope, not a scope reconstructed from a retired Parallel map.
// Corrections use reconcile instead, which never increments iterations.
func (a *correctionAccounts) book(scope string, u store.OutputCorrectionUsage, countIteration bool) error {
	if scope == "" || !validCorrectionUsage(u) {
		return errors.New("output correction: invalid execution usage")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if math.IsInf(a.state.DailyScopeCostUSD[scope]+u.CostUSD, 0) {
		return errors.New("output correction: daily accounting overflow")
	}
	if a.budget != nil {
		a.budget.mu.Lock()
		defer a.budget.mu.Unlock()
		unpriced, nodes, iterations := 0, 0, 0
		if countIteration {
			iterations = 1
		}
		if u.Tokens > 0 && u.CostUSD == 0 {
			unpriced, nodes = u.Tokens, 1
		}
		if err := checkCorrectionBudgetAddition(a.budget, u, unpriced, nodes, iterations); err != nil {
			return err
		}
		a.budget.tokensUsed += u.Tokens
		a.budget.costUsed += u.CostUSD
		a.budget.unreportedCalls += u.UnreportedCalls
		if countIteration {
			a.budget.iterationsUsed++
		}
		if u.Tokens > 0 && u.CostUSD == 0 {
			a.budget.unpricedTokens += u.Tokens
			a.budget.unpricedNodes++
		}
	}
	a.state.DailyScopeCostUSD[scope] += u.CostUSD
	return nil
}

// reconcile consumes current AND retired receipts. Budget and daily marks are
// independent: a budget checkpoint alone cannot certify a branch's daily base.
// It stages the whole update before touching counters, so malformed inputs do
// not partly apply. Snapshot takes the same lock before reading the budget.
func (a *correctionAccounts) reconcile(r *store.Run) error {
	if r == nil {
		return errors.New("output correction: missing accounting run")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	next := cloneCorrectionAccounts(a.state)
	var delta store.OutputCorrectionUsage
	unpriced, iterations, unpricedNodes := 0, 0, 0
	markInitial := func(res store.OutputCorrectionReservation) error {
		if res.Receipt == nil || res.Receipt.Observation == nil {
			return nil
		}
		initial, ok := r.InitialUsageReservations[res.InitialUsageID]
		if !ok || initial.ExecutionEpoch != res.ExecutionEpoch || initial.ScopeKey != res.ScopeKey {
			return errors.New("output correction: receipt without original invocation")
		}
		key := correctionAccountingKey("initial", initial.ID)
		iterationKey := key // Historical reservations retain their original accounting identity.
		if initial.LogicalInvocationID != "" {
			iterationKey = correctionAccountingKey("logical", r.ID, initial.LogicalInvocationID)
		}
		if res.Receipt.Observation.CountIteration && !next.IterationMarks[iterationKey] {
			next.IterationMarks[iterationKey] = true
			iterations++
		}
		if res.Receipt.Observation.UnpricedTokens > 0 && !next.UnpricedNodeMarks[key] {
			next.UnpricedNodeMarks[key] = true
			unpricedNodes++
		}
		return nil
	}
	for id, res := range r.InitialUsageReservations {
		if res.Receipt == nil {
			continue
		}
		if res.ID != id || res.InitialUsageID != id || res.ScopeKey == "" || res.Receipt.Observation == nil || !validCorrectionUsage(res.Receipt.Usage) {
			return errors.New("output correction: malformed initial receipt")
		}
		if _, err := store.FreezeSettledUsageReceipt(*res.Receipt); err != nil {
			return err
		}
		key := correctionAccountingKey("initial", id)
		u := res.Receipt.Usage
		old := next.UsageMarks[key]
		var addErr error
		delta, addErr = checkedCorrectionUsageSum(delta, store.OutputCorrectionUsage{Tokens: max(0, u.Tokens-old.Tokens), CostUSD: max(0, u.CostUSD-old.CostUSD), UnreportedCalls: max(0, u.UnreportedCalls-old.UnreportedCalls)})
		if addErr != nil {
			return addErr
		}
		next.UsageMarks[key] = store.OutputCorrectionUsage{Tokens: max(old.Tokens, u.Tokens), CostUSD: max(old.CostUSD, u.CostUSD), UnreportedCalls: max(old.UnreportedCalls, u.UnreportedCalls)}
		if err := addCorrectionCount(&unpriced, max(0, res.Receipt.Observation.UnpricedTokens-next.UnpricedMarks[key])); err != nil {
			return err
		}
		next.UnpricedMarks[key] = max(next.UnpricedMarks[key], res.Receipt.Observation.UnpricedTokens)
		next.DailyScopeCostUSD[res.ScopeKey] += max(0, u.CostUSD-next.DailyMarks[key])
		next.DailyMarks[key] = max(next.DailyMarks[key], u.CostUSD)
		if err := markInitial(res); err != nil {
			return err
		}
	}
	episodes := append([]store.OutputCorrectionEpisode(nil), r.OutputCorrectionHistory...)
	for key, ep := range r.OutputCorrections {
		ep.LedgerKey = key
		episodes = append(episodes, ep)
	}
	for _, ep := range episodes {
		if ep.Generation == "" {
			continue
		} // legacy had no fenced receipts
		if ep.LedgerKey == "" || !validCorrectionUsage(ep.Usage) {
			return errors.New("output correction: malformed accounting episode")
		}
		key := correctionAccountingKey(ep.LedgerKey, ep.Generation)
		old := next.UsageMarks[key]
		var addErr error
		delta, addErr = checkedCorrectionUsageSum(delta, store.OutputCorrectionUsage{Tokens: max(0, ep.Usage.Tokens-old.Tokens), CostUSD: max(0, ep.Usage.CostUSD-old.CostUSD), UnreportedCalls: max(0, ep.Usage.UnreportedCalls-old.UnreportedCalls)})
		if addErr != nil {
			return addErr
		}
		next.UsageMarks[key] = store.OutputCorrectionUsage{Tokens: max(old.Tokens, ep.Usage.Tokens), CostUSD: max(old.CostUSD, ep.Usage.CostUSD), UnreportedCalls: max(old.UnreportedCalls, ep.Usage.UnreportedCalls)}
		scopes := map[string]float64{}
		episodeUnpriced := 0
		for _, res := range ep.Reservations {
			if res.Receipt == nil {
				continue
			}
			if res.ScopeKey == "" || !validCorrectionUsage(res.Receipt.Usage) {
				return errors.New("output correction: malformed accounting receipt")
			}
			scopes[res.ScopeKey] += res.Receipt.Usage.CostUSD
			if res.Receipt.Observation != nil {
				if _, err := store.FreezeSettledUsageReceipt(*res.Receipt); err != nil {
					return err
				}
				if err := addCorrectionCount(&episodeUnpriced, res.Receipt.Observation.UnpricedTokens); err != nil {
					return err
				}
				if err := markInitial(res); err != nil {
					return err
				}
			} else if res.Receipt.Usage.CostUSD == 0 {
				if err := addCorrectionCount(&episodeUnpriced, res.Receipt.Usage.Tokens); err != nil {
					return err
				}
			}
		}
		if err := addCorrectionCount(&unpriced, max(0, episodeUnpriced-next.UnpricedMarks[key])); err != nil {
			return err
		}
		next.UnpricedMarks[key] = max(next.UnpricedMarks[key], episodeUnpriced)
		for scope, total := range scopes {
			mark := correctionAccountingKey(ep.LedgerKey, ep.Generation, scope)
			next.DailyScopeCostUSD[scope] += max(0, total-next.DailyMarks[mark])
			next.DailyMarks[mark] = max(total, next.DailyMarks[mark])
		}
	}
	for _, v := range next.DailyScopeCostUSD {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return errors.New("output correction: daily accounting overflow")
		}
	}
	if !validCorrectionUsage(delta) {
		return errors.New("output correction: usage overflow")
	}
	if a.budget != nil {
		a.budget.mu.Lock()
		defer a.budget.mu.Unlock()
		if err := checkCorrectionBudgetAddition(a.budget, delta, unpriced, unpricedNodes, iterations); err != nil {
			return err
		}
		a.budget.tokensUsed += delta.Tokens
		a.budget.costUsed += delta.CostUSD
		a.budget.unreportedCalls += delta.UnreportedCalls
		a.budget.unpricedTokens += unpriced
		a.budget.unpricedNodes += unpricedNodes
		a.budget.iterationsUsed += iterations
		// The original node owns its single iteration/unpriced-node count.
	}
	a.state = next
	return nil
}

// snapshot must be the checkpoint builder's single read for both the budget
// and accounting marks. Wire callers embed the returned carrier and counters
// together; serializing marks after a separate budget.Snapshot is incorrect.
func (a *correctionAccounts) snapshot(cp *store.Checkpoint) store.CorrectionAccounting {
	a.mu.Lock()
	defer a.mu.Unlock()
	tokens, cost, iterations, elapsed, unpriced, unpricedNodes, unreported := a.budget.Snapshot()
	cp.BudgetTokensUsed, cp.BudgetCostUSD, cp.BudgetIterationsUsed = tokens, cost, iterations
	cp.BudgetUnpricedTokens, cp.BudgetUnpricedNodes, cp.BudgetUnreportedCalls = unpriced, unpricedNodes, unreported
	cp.BudgetElapsedNS = elapsed.Nanoseconds()
	return cloneCorrectionAccounts(a.state)
}

// raiseDailyBases imports the legacy scopes that still exist and today's
// durable daily high-waters. It cannot reconstruct an already-removed legacy
// branch absent from today's ledger; that historical limitation stays explicit.
func (a *correctionAccounts) raiseDailyBases(bases map[string]float64) error {
	for scope, v := range bases {
		if scope == "" || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return errors.New("output correction: invalid daily base")
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for scope, v := range bases {
		a.state.DailyScopeCostUSD[scope] = max(a.state.DailyScopeCostUSD[scope], v)
	}
	return nil
}

// restoreDaily imports only known scopes. A historical branch which vanished
// before upgrade cannot be inferred from unrelated runs in a daily ledger.
func (a *correctionAccounts) restoreDaily(ctx context.Context, g *DailyCapGuard, legacy map[string]float64) error {
	if err := a.raiseDailyBases(legacy); err != nil {
		return err
	}
	if g == nil {
		return nil
	}
	ds, err := g.store.LoadDailySpend(ctx, g.today())
	if err != nil {
		return err
	}
	a.mu.Lock()
	bases := map[string]float64{}
	for scope := range a.state.DailyScopeCostUSD {
		bases[scope] = ds.RunsContributed[scope]
	}
	a.mu.Unlock()
	return a.raiseDailyBases(bases)
}
func (a *correctionAccounts) flushDaily(ctx context.Context, g *DailyCapGuard) error {
	if g == nil {
		return nil
	}
	a.mu.Lock()
	scopes := cloneMap(a.state.DailyScopeCostUSD)
	a.mu.Unlock()
	for scope, value := range scopes {
		if _, err := g.Record(ctx, scope, value); err != nil {
			return err
		}
	}
	return nil
}
