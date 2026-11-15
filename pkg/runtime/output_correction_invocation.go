package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

type correctionInvocation struct {
	genericOrdinal      uint64
	genericCollectionID string
	nodeID              string
	generic             bool
	initial             store.OutputCorrectionReservation
	collector           *model.ExecutionUsageCollector
}
type correctionInvocationKey struct{}

func (e *Engine) correctionAccountingCapable() bool {
	p, ok := e.executor.(interface{ ExecutionAccountingVersion() int })
	return ok && p.ExecutionAccountingVersion() == model.ExecutionAccountingProtocol && store.AsObservedExecutionStore(e.store) != nil
}
func correctionObservation(u model.ExecutionUsage, countIteration bool) *store.UsageObservation {
	out := &store.UsageObservation{Version: 2, CollectionID: u.CollectionID, Components: store.CloneUsageComponents(u.Components), Tokens: u.Tokens, UnpricedTokens: u.UnpricedTokens, UnreportedCalls: u.UnreportedCalls, OtherCostUSD: u.OtherCostUSD, CountIteration: countIteration}
	for _, s := range u.Sessions {
		out.Sessions = append(out.Sessions, store.SessionCostObservation{Backend: s.Backend, SessionID: s.SessionID, Fingerprint: s.Fingerprint, CostUSD: s.CostUSD})
	}
	return out
}
func executionUsageSpent(u model.ExecutionUsage) bool {
	if u.Tokens > 0 || u.OtherCostUSD > 0 {
		return true
	}
	for _, s := range u.Sessions {
		if s.CostUSD > 0 {
			return true
		}
	}
	return false
}
func (e *Engine) executeCorrectionInvocation(ctx context.Context, rs *runState, nodeID, scope string, node ir.Node, input map[string]any) (map[string]any, error) {
	rs.correctionInvocation = nil
	x := correctionExecutionFromContext(ctx)
	_, agent := node.(*ir.AgentNode)
	_, judge := node.(*ir.JudgeNode)
	generic := e.genericCorrectionCapable() && ir.NodeOutputSchema(node) != ""
	if x == nil || (!generic && (!e.correctionAccountingCapable() || (!agent && !judge))) {
		// A child run or a nonparticipating executor must not inherit a caller's
		// collector or producer ownership. No paid schema handoff without a bill.
		ctx = model.WithExecutionUsageCollector(ctx, nil)
		ctx = model.WithoutOutputCorrectionOwnership(ctx)
		return e.executeWithResources(ctx, rs, node, input)
	}
	if x.authority.RunID != rs.runID {
		return nil, errCorrectionOwnershipLost
	}
	initial := store.OutputCorrectionReservation{ID: store.NewExecutionEpoch(), ExecutionEpoch: x.authority.Epoch, ScopeKey: scope, StartedAt: time.Now().UTC()}
	initial.InitialUsageID = initial.ID
	_, initial.LogicalInvocationID = e.correctionInvocationIdentity(rs, nodeID)
	origin, originErr := usageOriginForRun(ctx, rs.runID)
	if originErr != nil {
		return nil, originErr
	}
	initial.Origin = origin
	ledger := store.AsObservedExecutionStore(x.runs)
	if ledger == nil {
		return nil, errors.New("output correction: incomplete observation store")
	}
	reserved, err := ledger.ReserveInitialUsageIf(ctx, rs.runID, x.authority, initial)
	if err != nil {
		return nil, fmt.Errorf("reserve initial usage: %w", err)
	}
	if !reserved {
		return nil, errCorrectionOwnershipLost
	}
	invocation := &correctionInvocation{nodeID: nodeID, initial: initial, collector: model.NewExecutionUsageCollector(), generic: generic}
	if generic {
		invocation.genericCollectionID = store.NewUsageCollectionID()
	}
	rs.correctionInvocation = invocation
	ctx = context.WithValue(x.context(ctx), correctionInvocationKey{}, invocation)
	ctx = model.WithExecutionUsageCollector(ctx, invocation.collector)
	execCtx := ctx
	if generic {
		execCtx = model.WithoutOutputCorrectionOwnership(model.WithExecutionUsageCollector(ctx, nil))
	}
	output, execErr := e.executeWithResources(execCtx, rs, node, input)
	usage, usageErr := invocation.collector.Snapshot()
	if generic {
		invocation.genericOrdinal++
		usage = genericExecutionUsage(output, invocation.genericCollectionID, invocation.genericOrdinal)
	}
	settleCtx, cancel := detachedBookingCtx(ctx)
	defer cancel()
	if usageErr == nil {
		_, usageErr = ledger.RecordOutputCorrectionReceipt(settleCtx, rs.runID, store.InitialUsageTarget(initial), store.OutputCorrectionReceipt{Observation: correctionObservation(usage, execErr == nil || executionUsageSpent(usage))})
	}
	if usageErr == nil {
		usageErr = e.reconcileExecutionAccounts(settleCtx, rs, x)
	}
	if usageErr != nil {
		releaseCorrectionFailure(execErr)
		return output, errors.Join(execErr, fmt.Errorf("settle initial usage: %w", usageErr))
	}
	// Settlement never grants permission to publish under a successor's epoch.
	if _, _, err := x.load(settleCtx, ""); err != nil {
		releaseCorrectionFailure(execErr)
		return output, errors.Join(execErr, err)
	}
	if generic {
		if execErr != nil {
			return output, execErr
		}
		return e.correctAndValidateNodeOutputInScope(ctx, rs, nodeID, scope, node, output)
	}
	output, execErr = e.correctFailedExecution(ctx, rs, nodeID, scope, node, output, execErr)
	return output, schemaExecutionError(node, execErr)
}
func releaseCorrectionFailure(err error) {
	var failure *model.OutputCorrectionFailure
	if errors.As(err, &failure) {
		failure.Handle().Release()
	}
}
func (e *Engine) reconcileExecutionAccounts(ctx context.Context, rs *runState, x *correctionExecution) error {
	r, err := x.runs.LoadRun(ctx, rs.runID)
	if err != nil {
		return err
	}
	if rs.correctionAccounts == nil {
		return errors.New("output correction: absent accounting carrier")
	}
	if err = rs.correctionAccounts.reconcile(r); err != nil {
		return err
	}
	return rs.correctionAccounts.flushDaily(ctx, e.dailyCap)
}
func accountedInvocation(rs *runState, nodeID string) bool {
	return rs != nil && rs.correctionInvocation != nil && rs.correctionInvocation.nodeID == nodeID
}
