package runtime

import (
	"context"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
	"time"
)

// genericCorrectionCapable is the opt-in legacy corrector contract. An executor
// advertising a producer protocol must use that protocol, never this fallback.
func (e *Engine) genericCorrectionCapable() bool {
	if !e.validateOutputs || store.AsObservedExecutionStore(e.store) == nil {
		return false
	}
	if _, producer := e.executor.(interface{ ExecutionAccountingVersion() int }); producer {
		return false
	}
	_, plain := e.executor.(OutputCorrector)
	_, usage := e.executor.(OutputCorrectorWithUsage)
	return plain || usage
}
func (e *Engine) correctionProtocolCapable() bool {
	return e.correctionAccountingCapable() || e.genericCorrectionCapable()
}
func genericExecutionUsage(output map[string]any, collectionID string, ordinal uint64) model.ExecutionUsage {
	s := extractSpend(output)
	u := model.ExecutionUsage{CollectionID: collectionID, Tokens: s.tokens, OtherCostUSD: s.costUSD, UnreportedCalls: s.unreportedCalls}
	if s.costUSD == 0 {
		u.UnpricedTokens = s.tokens
	}
	u.Components = []store.UsageComponent{{Ordinal: ordinal, Source: store.UsageGenericReported, Tokens: u.Tokens, UnpricedTokens: u.UnpricedTokens, UnreportedCalls: u.UnreportedCalls, CostKnown: u.OtherCostUSD > 0, CostKind: store.UsageAdditiveCost, CostUSD: u.OtherCostUSD}}
	if key, total := retrySessionCost(output); total && key.backend != "" {
		fingerprint, _ := output["_session_fingerprint"].(string)
		u.Components[0].CostKind = store.UsageSessionTotal
		u.Components[0].ReportedSession = &store.ReportedEconomicSession{Backend: key.backend, SessionID: key.session, Fingerprint: fingerprint}
		u.Sessions = []model.OutputCorrectionSessionCost{{Backend: key.backend, SessionID: key.session, Fingerprint: fingerprint, CostUSD: s.costUSD}}
		u.OtherCostUSD = 0
	}
	return u
}

// reserveGenericOriginal covers internally executed pure nodes reaching the
// validation adapter. Delegated/custom Execute calls reserve in the common
// dispatch before their effects. This helper never acquires execution authority.
func (e *Engine) reserveGenericOriginal(ctx context.Context, rs *runState, nodeID, scope string, output map[string]any) error {
	x := correctionExecutionFromContext(ctx)
	if x == nil || x.authority.RunID != rs.runID {
		return errCorrectionOwnershipLost
	}
	origin, err := usageOriginForRun(ctx, rs.runID)
	if err != nil {
		return err
	}
	res := store.OutputCorrectionReservation{ID: store.NewExecutionEpoch(), ExecutionEpoch: x.authority.Epoch, ScopeKey: scope, StartedAt: time.Now().UTC(), Origin: origin}
	res.InitialUsageID = res.ID
	_, res.LogicalInvocationID = e.correctionInvocationIdentity(rs, nodeID)
	ledger := store.AsObservedExecutionStore(x.runs)
	if ledger == nil {
		return fmt.Errorf("output correction: incomplete generic accounting store")
	}
	ok, err := ledger.ReserveInitialUsageIf(ctx, rs.runID, x.authority, res)
	if err != nil {
		return err
	}
	if !ok {
		return errCorrectionOwnershipLost
	}
	rs.correctionInvocation = &correctionInvocation{nodeID: nodeID, initial: res, generic: true, genericOrdinal: 1, genericCollectionID: store.NewUsageCollectionID()}
	usage := genericExecutionUsage(output, rs.correctionInvocation.genericCollectionID, 1)
	if _, err = ledger.RecordOutputCorrectionReceipt(ctx, rs.runID, store.InitialUsageTarget(res), store.OutputCorrectionReceipt{Observation: correctionObservation(usage, true)}); err != nil {
		return err
	}
	return e.reconcileExecutionAccounts(ctx, rs, x)
}
func (e *Engine) correctGenericOutput(ctx context.Context, rs *runState, nodeID, scope string, node ir.Node, output map[string]any, validation error) (map[string]any, error) {
	if !e.genericCorrectionCapable() {
		return output, validation
	}
	x := correctionExecutionFromContext(ctx)
	if x == nil || x.authority.RunID != rs.runID {
		return output, errors.Join(validation, errCorrectionOwnershipLost)
	}
	if !accountedInvocation(rs, nodeID) {
		if err := e.reserveGenericOriginal(ctx, rs, nodeID, scope, output); err != nil {
			return output, err
		}
	}
	invocation := rs.correctionInvocation
	if !invocation.generic {
		return output, validation
	}
	key, id := e.correctionInvocationIdentity(rs, nodeID)
	bounded, cancel, budgetDeadline, err := e.outputCorrectionContext(ctx, rs, nodeID)
	if err != nil {
		return output, err
	}
	defer cancel()
	q := correctionRequest{Key: key, InvocationID: id, NodeID: nodeID, ScopeKey: scope, InitialUsageID: invocation.initial.ID, Budget: e.outputCorrectionBudget, Output: output,
		Validate:        func(candidate map[string]any) error { return e.validateNodeOutput(rs, nodeID, node, candidate) },
		BeforeRound:     func(candidate map[string]any) error { return e.outputCorrectionSpendError(rs, nodeID, candidate) },
		Scrub:           func(err error) string { return e.recordedFailureText(err.Error()) },
		ReceiptRecorded: func(c context.Context) error { return e.reconcileExecutionAccounts(c, rs, x) },
	}
	q.Announce = func(ep store.OutputCorrectionEpisode, phase string) {
		e.emitOutputCorrectionEvent(rs, nodeID, ep, phase)
	}
	latest := model.ExecutionUsage{CollectionID: invocation.genericCollectionID}
	q.Observation = func() (*store.UsageObservation, error) {
		return correctionObservation(latest, executionUsageSpent(latest)), nil
	}
	q.Round = func(c context.Context, current map[string]any, invalid error) (map[string]any, store.OutputCorrectionUsage, error) {
		if resource := e.resourceScope; resource != nil {
			if err := resource.gate.sem.Acquire(c, 1); err != nil {
				return nil, store.OutputCorrectionUsage{}, err
			}
			defer resource.gate.sem.Release(1)
		}
		c = model.WithoutOutputCorrectionOwnership(model.WithExecutionUsageCollector(c, nil))
		var candidate map[string]any
		var usage store.OutputCorrectionUsage
		var callErr error
		if corrector, ok := e.executor.(OutputCorrectorWithUsage); ok {
			candidate, usage, callErr = corrector.CorrectOutputWithUsage(c, node, current, invalid)
		} else {
			candidate, callErr = e.executor.(OutputCorrector).CorrectOutput(c, node, current, invalid)
		}
		latest = model.ExecutionUsage{CollectionID: invocation.genericCollectionID, Tokens: usage.Tokens, OtherCostUSD: usage.CostUSD, UnreportedCalls: usage.UnreportedCalls}
		if usage.CostUSD == 0 {
			latest.UnpricedTokens = usage.Tokens
		}
		invocation.genericOrdinal++
		latest.Components = []store.UsageComponent{{Ordinal: invocation.genericOrdinal, Source: store.UsageGenericReported, Tokens: latest.Tokens, UnpricedTokens: latest.UnpricedTokens, UnreportedCalls: latest.UnreportedCalls, CostKnown: latest.OtherCostUSD > 0, CostKind: store.UsageAdditiveCost, CostUSD: latest.OtherCostUSD}}
		if candidate == nil {
			return nil, usage, callErr
		}
		return preserveCorrectionMetadata(current, candidate, usage), usage, callErr
	}
	result, err := x.correct(bounded, q)
	if budgetDeadline && ctx.Err() == nil && errors.Is(bounded.Err(), context.DeadlineExceeded) {
		return result, errors.Join(err, outputCorrectionDurationError(nodeID))
	}
	return result, err
}
