package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

type correctionExecutionKey struct{}

// correctFailedExecution is the pre-generic-failure adapter. Until the owned
// Engine claim/dispatch sites are composed it is intentionally not called.
// The context carries the acquired grant; the adapter never loads an epoch.
func (e *Engine) correctFailedExecution(ctx context.Context, rs *runState, nodeID, dailyScope string, node ir.Node, output map[string]any, executionErr error) (map[string]any, error) {
	var failure *model.OutputCorrectionFailure
	if !errors.As(executionErr, &failure) {
		return output, executionErr
	}
	h := failure.Handle()
	defer h.Release()
	x := correctionExecutionFromContext(ctx)
	if x == nil || x.authority.RunID != rs.runID || h.Ownership().Authority != x.authority {
		return output, errors.Join(executionErr, errCorrectionOwnershipLost)
	}
	corrector, ok := e.executor.(interface {
		CorrectOutputWithUsage(context.Context, ir.Node, map[string]any, error) (map[string]any, store.OutputCorrectionUsage, error)
	})
	if !ok {
		return output, executionErr
	}
	schemaName := ir.NodeOutputSchema(node)
	schema := e.workflow.Schemas[schemaName]
	if schemaName == "" || schema == nil {
		return output, fmt.Errorf("output correction: missing schema for node %q", nodeID)
	}
	key, invocation := e.correctionInvocationIdentity(rs, nodeID)
	q := correctionRequest{Key: key, InvocationID: invocation, NodeID: nodeID, ScopeKey: dailyScope, Budget: e.outputCorrectionBudget, Output: failure.Output(), ErrorOnRejection: executionErr,
		RejectionError: func(status string, _ error) error {
			return &model.OutputSchemaError{Cause: executionErr, Unchanged: status == correctionStatusUnchanged}
		},
		PendingStep: h.HasPendingExtraction,
		Validate:    func(out map[string]any) error { return model.ValidateOutput(out, schema) },
		Scrub:       func(err error) string { return e.recordedFailureText(err.Error()) },
		BeforeRound: func(out map[string]any) error { return e.outputCorrectionSpendError(rs, nodeID, out) },
	}
	q.Announce = func(ep store.OutputCorrectionEpisode, phase string) {
		e.emitOutputCorrectionEvent(rs, nodeID, ep, phase)
	}
	usageInvocation, _ := ctx.Value(correctionInvocationKey{}).(*correctionInvocation)
	var before model.ExecutionUsage
	if usageInvocation != nil {
		q.InitialUsageID = usageInvocation.initial.ID
		q.Observation = func() (*store.UsageObservation, error) {
			u, err := usageInvocation.collector.Since(before)
			if err != nil {
				return nil, err
			}
			return correctionObservation(u, executionUsageSpent(u)), nil
		}
		q.ReceiptRecorded = func(c context.Context) error { return e.reconcileExecutionAccounts(c, rs, x) }
	}
	q.Round = func(roundCtx context.Context, out map[string]any, validation error) (map[string]any, store.OutputCorrectionUsage, error) {
		if usageInvocation != nil {
			var err error
			before, err = usageInvocation.collector.Snapshot()
			if err != nil {
				return nil, store.OutputCorrectionUsage{}, err
			}
		}
		// Needs leases remain held by the surrounding invocation. This reader
		// gate is separate and protects the child's shared workspace each round.
		if scope := e.resourceScope; scope != nil {
			if err := scope.gate.sem.Acquire(roundCtx, 1); err != nil {
				return nil, store.OutputCorrectionUsage{}, err
			}
			defer scope.gate.sem.Release(1)
		}
		candidate, usage, err := corrector.CorrectOutputWithUsage(model.WithOutputCorrectionHandle(roundCtx, h), node, out, validation)
		if candidate == nil {
			candidate = h.Output()
		}
		return candidate, usage, err
	}
	if finalizer, ok := e.executor.(interface {
		FinalizeCorrectedOutput(context.Context, ir.Node, map[string]any) error
	}); ok {
		q.Finalize = func(c context.Context, out map[string]any) error {
			return finalizer.FinalizeCorrectedOutput(model.WithOutputCorrectionHandle(x.context(c), h), node, out)
		}
	}
	return x.correct(ctx, q)
}
