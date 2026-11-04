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

type outputValidator interface{ ValidatesOutput(ir.Node) bool }

func (e *Engine) executorValidatesOutput(node ir.Node) bool {
	v, ok := e.executor.(outputValidator)
	return ok && v.ValidatesOutput(node)
}

// executorCorrectionContext gives the canonical executor loop the same durable
// allowance as runtime correctors. This is per invocation, never executor state:
// parallel branches must neither share nor reset one another's allowance.
func (e *Engine) executorCorrectionContext(ctx context.Context, rs *runState, node ir.Node) (context.Context, context.CancelFunc, error) {
	noop := func() {}
	if rs == nil || !e.executorValidatesOutput(node) {
		return ctx, noop, nil
	}
	nodeID := node.NodeID()
	budgetDeadline := false
	key, invocation := e.correctionInvocationIdentity(rs, nodeID)
	var episode store.OutputCorrectionEpisode
	loaded := false
	persist := func(callCtx context.Context, output map[string]any, validationErr error, phase string) error {
		episode.LastOutputFingerprint = correctionSemanticFingerprint(output)
		episode.UpdatedAt = time.Now().UTC()
		episode.LastError = ""
		if validationErr != nil {
			episode.LastError = validationErr.Error()
			episode.LastViolationFingerprint = correctionFingerprint(validationErr.Error())
		}
		// A cancelled call still spent its reserved attempt. The existing
		// granular store seam provides both filesystem and Mongo durability.
		persistCtx := callCtx
		cancelPersist := noop
		if callCtx.Err() != nil {
			persistCtx, cancelPersist = context.WithTimeout(context.WithoutCancel(callCtx), 5*time.Second)
		}
		defer cancelPersist()
		if err := e.persistCorrectionEpisode(persistCtx, rs.runID, key, episode); err != nil {
			return fmt.Errorf("output correction ledger: %w", err)
		}
		e.emitOutputCorrectionEvent(rs, nodeID, episode, phase)
		return nil
	}
	policy := model.OutputCorrectionPolicy{
		Context: func(callCtx context.Context) (context.Context, context.CancelFunc, error) {
			bounded, cancel, budgetBound, err := e.outputCorrectionContext(callCtx, rs, nodeID)
			budgetDeadline = budgetBound
			return bounded, cancel, err
		},
		Before: func(callCtx context.Context, output map[string]any, validationErr error) error {
			if callCtx.Err() != nil {
				return callCtx.Err()
			}
			if store.AsOutputCorrectionStore(e.store) == nil {
				return &model.OutputSchemaError{Cause: validationErr}
			}
			if !loaded {
				var found bool
				var err error
				episode, found, err = e.loadInvocationCorrectionEpisode(callCtx, rs, nodeID, key, invocation)
				if err != nil {
					return fmt.Errorf("output correction ledger read: %w", err)
				}
				if !found {
					episode = store.OutputCorrectionEpisode{EpisodeID: key, InvocationID: invocation, NodeID: nodeID, Budget: e.outputCorrectionBudget, Status: correctionStatusActive, InputFingerprint: correctionSemanticFingerprint(output), StartedAt: time.Now().UTC()}
				} else if episode.Budget < e.outputCorrectionBudget {
					episode.Budget = e.outputCorrectionBudget
					if episode.Status == correctionStatusExhausted {
						episode.Status = correctionStatusActive
					}
				}
				loaded = true
			}
			if episode.Status == correctionStatusUnchanged {
				return &model.OutputSchemaError{Cause: validationErr, Unchanged: true}
			}
			if e.outputCorrectionBudget == 0 || episode.Attempts >= episode.Budget {
				episode.Status = correctionStatusExhausted
				if err := persist(callCtx, output, validationErr, episode.Status); err != nil {
					return err
				}
				return &model.OutputSchemaError{Cause: validationErr}
			}
			if err := e.outputCorrectionSpendError(rs, nodeID, output); err != nil {
				episode.Status = correctionStatusSpendBlocked
				if persistErr := persist(callCtx, output, err, episode.Status); persistErr != nil {
					return persistErr
				}
				return err
			}
			episode.Attempts++
			episode.Status = correctionStatusActive
			return persist(callCtx, output, validationErr, "attempt")
		},
		After: func(callCtx context.Context, output map[string]any, outcome error) error {
			episode.Status = correctionStatusActive
			var schemaErr *model.OutputSchemaError
			if errors.As(outcome, &schemaErr) && schemaErr.Unchanged {
				episode.Status = correctionStatusUnchanged
			} else if outcome == nil {
				episode.Status = correctionStatusSucceeded
			} else if episode.Attempts >= episode.Budget {
				episode.Status = correctionStatusExhausted
			}
			if err := persist(callCtx, output, outcome, episode.Status); err != nil {
				return err
			}
			if budgetDeadline && errors.Is(callCtx.Err(), context.DeadlineExceeded) {
				return outputCorrectionDurationError(nodeID)
			}
			return nil
		},
	}
	return model.WithOutputCorrectionPolicy(ctx, policy), noop, nil
}

func schemaExecutionError(node ir.Node, err error) error {
	if ClassifyError(err) == ErrCodeSchemaValidation {
		return &RuntimeError{Code: ErrCodeSchemaValidation, NodeID: node.NodeID(), Message: err.Error(), Cause: err}
	}
	return err
}
