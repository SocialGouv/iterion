package runtime

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// prepareCorrectionAccounting reconciles only into an invocation-local copy of
// the checkpoint. It changes neither authority nor cursor. Public entry calls
// it before effects; a successful resume claim repeats it on its fresh snapshot
// before any spend gate or workspace restore.
func (e *Engine) prepareCorrectionAccounting(ctx context.Context, r *store.Run) error {
	if !e.correctionProtocolCapable() || r == nil {
		return nil
	}
	if r.Checkpoint == nil && len(r.InitialUsageReservations) == 0 && len(r.OutputCorrections) == 0 && len(r.OutputCorrectionHistory) == 0 {
		return nil
	}
	cp := &store.Checkpoint{NodeID: e.workflow.Entry}
	if r.Checkpoint != nil {
		copy := *r.Checkpoint
		cp = &copy
	}
	if err := validateCorrectionCheckpoint(cp); err != nil {
		return err
	}
	b := e.newRunBudget()
	if b == nil {
		b = &SharedBudget{logger: e.logger, startedAt: time.Now(), warningsEmitted: map[string]bool{}}
	}
	b.Restore(cp.BudgetTokensUsed, cp.BudgetCostUSD, cp.BudgetIterationsUsed, time.Duration(cp.BudgetElapsedNS), cp.BudgetUnpricedTokens, cp.BudgetUnpricedNodes, cp.BudgetUnreportedCalls)
	a, err := newCorrectionAccounts(b, cp.CorrectionAccounting)
	if err != nil {
		return err
	}
	bases := map[string]float64{r.ID: cp.CostUSDTotal}
	if cp.Parallel != nil {
		for id, branch := range cp.Parallel.Branches {
			if branch != nil {
				bases[branchLedgerKey(r.ID, id, &runState{loopCounters: cp.LoopCounters})] = branch.CostUSD
			}
		}
	}
	if err = a.restoreDaily(ctx, e.dailyCap, bases); err != nil {
		return err
	}
	if err = a.reconcile(r); err != nil {
		return err
	}
	snapshot := a.snapshot(cp)
	cp.CorrectionAccounting = &snapshot
	if r.Checkpoint == nil {
		r.Checkpoint = cp
	} else {
		copyCorrectionAccountingFields(r.Checkpoint, cp)
	}
	return nil
}
func copyCorrectionAccountingFields(dst, src *store.Checkpoint) {
	dst.BudgetTokensUsed = src.BudgetTokensUsed
	dst.BudgetCostUSD = src.BudgetCostUSD
	dst.BudgetIterationsUsed = src.BudgetIterationsUsed
	dst.BudgetElapsedNS = src.BudgetElapsedNS
	dst.BudgetUnpricedTokens = src.BudgetUnpricedTokens
	dst.BudgetUnpricedNodes = src.BudgetUnpricedNodes
	dst.BudgetUnreportedCalls = src.BudgetUnreportedCalls
	dst.CorrectionAccounting = src.CorrectionAccounting
}
func validateCorrectionCheckpoint(cp *store.Checkpoint) error {
	if cp.BudgetTokensUsed < 0 || cp.BudgetIterationsUsed < 0 || cp.BudgetUnpricedTokens < 0 || cp.BudgetUnpricedNodes < 0 || cp.BudgetUnreportedCalls < 0 || cp.BudgetElapsedNS < 0 || cp.BudgetCostUSD < 0 || math.IsNaN(cp.BudgetCostUSD) || math.IsInf(cp.BudgetCostUSD, 0) {
		return errors.New("output correction: invalid checkpoint accounting")
	}
	return nil
}
