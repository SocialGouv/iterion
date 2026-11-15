package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// correctionRequest binds one original Execute to the durable ledger. Round
// must return marginal usage even on failure. A successful candidate already
// contains the producer's complete output metadata; the runtime never folds
// that aggregate a second time. ErrorOnRejection preserves the producer cause.
type correctionRequest struct {
	Key, InvocationID, NodeID, ScopeKey string
	Budget                              int
	InitialUsageID                      string
	Observation                         func() (*store.UsageObservation, error)
	Output                              map[string]any
	ErrorOnRejection                    error
	// RejectionError adapts terminal schema disposition without changing generic cause identity.
	RejectionError func(status string, validation error) error
	// PendingStep distinguishes a new correction operation from another identical
	// answer retry. It never bypasses the durable attempt budget or reservation.
	PendingStep func() bool
	Validate    func(map[string]any) error
	Round       func(context.Context, map[string]any, error) (map[string]any, store.OutputCorrectionUsage, error)
	BeforeRound func(map[string]any) error
	Finalize    func(context.Context, map[string]any) error
	Scrub       func(error) string
	// ReceiptRecorded is accounting only. Failure stops before another call.
	ReceiptRecorded func(context.Context) error
	Announce        func(store.OutputCorrectionEpisode, string)
}

func (q correctionRequest) rejection(status string, validation error) error {
	if q.RejectionError != nil {
		return q.RejectionError(status, validation)
	}
	if q.ErrorOnRejection != nil {
		return q.ErrorOnRejection
	}
	return validation
}
func (q correctionRequest) safeError(err error) string {
	if err == nil {
		return ""
	}
	if q.Scrub != nil {
		return q.Scrub(err)
	}
	return "output correction stopped"
}

// correct runs the fenced candidate protocol. It never retries a paid call on
// any storage conflict, and never refreshes x.authority. The sole receipt target
// is captured by value before Round and remains valid for retired generations.
func (x *correctionExecution) correct(ctx context.Context, q correctionRequest) (map[string]any, error) {
	if q.Key == "" || q.ScopeKey == "" || q.Validate == nil || q.Round == nil {
		return q.Output, errors.New("output correction: incomplete invocation")
	}
	decide := func(c context.Context, key string, old store.OutputCorrectionEpisode, exists bool, next store.OutputCorrectionEpisode) (store.OutputCorrectionEpisode, error) {
		stored, err := x.decision(c, key, old, exists, next)
		if err == nil && q.Announce != nil {
			phase := ""
			switch {
			case next.Attempts > old.Attempts:
				phase = "attempt"
			case exists && next.Budget > old.Budget:
				phase = "budget_raised"
			case next.Status != old.Status && exists:
				phase = next.Status
			}
			if phase != "" {
				q.Announce(stored, phase)
			}
		}
		return stored, err
	}
	current := q.Output
	validation := q.Validate(current)
	if validation == nil {
		return current, nil
	}
	if err := ctx.Err(); err != nil {
		return current, err
	}
	ep, exists, err := x.load(ctx, q.Key)
	if err != nil {
		return current, err
	}
	if exists && ep.Cause != nil {
		return current, rehydrateCorrectionCause(ep.Cause)
	}
	if q.Budget <= 0 {
		return current, q.rejection(ep.Status, validation)
	}
	if !exists {
		now := time.Now().UTC()
		ep, err = decide(ctx, q.Key, store.OutputCorrectionEpisode{}, false, store.OutputCorrectionEpisode{
			Generation: store.NewExecutionEpoch(), EpisodeID: store.NewExecutionEpoch(), InvocationID: q.InvocationID, NodeID: q.NodeID,
			Budget: q.Budget, Status: correctionStatusActive, StartedAt: now,
			InputFingerprint: correctionSemanticFingerprint(current),
		})
		if err != nil {
			return current, err
		}
	} else {
		if ep.Cause != nil {
			return current, rehydrateCorrectionCause(ep.Cause)
		}
		next := ep
		// A legacy ledger retains its attempt count, but is not allowed to reset
		// an unknown terminal reason on an operator's budget raise.
		if next.Generation == "" {
			next.Generation = store.NewExecutionEpoch()
		}
		if q.Budget > ep.Budget && (ep.Status == correctionStatusExhausted || ep.Status == correctionStatusUnchanged) && ep.TerminalKind == "candidate_rejected" {
			next.Status = correctionStatusActive
			next.TerminalKind = ""
		}
		if q.Budget != ep.Budget || next.Generation != ep.Generation || next.Status != ep.Status {
			next.Budget = q.Budget
			ep, err = decide(ctx, q.Key, ep, true, next)
			if err != nil {
				return current, err
			}
		}
	}
	for {
		if ep.Status == correctionStatusSucceeded || ep.Status == correctionStatusUnchanged || ep.Status == correctionStatusExhausted {
			return current, q.rejection(ep.Status, validation)
		}
		if err := ctx.Err(); err != nil {
			return current, err
		}
		if q.BeforeRound != nil {
			if stop := q.BeforeRound(current); stop != nil {
				next := ep
				next.Status = correctionStatusSpendBlocked
				next.TerminalKind = "spend_blocked"
				next.LastError = q.safeError(stop)
				_, err = decide(ctx, q.Key, ep, true, next)
				return current, errors.Join(stop, err)
			}
		}
		if ep.Attempts >= q.Budget {
			next := ep
			next.Status = correctionStatusExhausted
			next.TerminalKind = "candidate_rejected"
			next.LastError = q.safeError(validation)
			_, err = decide(ctx, q.Key, ep, true, next)
			return current, errors.Join(q.rejection(next.Status, validation), err)
		}
		if ep.Status == correctionStatusSpendBlocked {
			next := ep
			next.Status = correctionStatusActive
			next.TerminalKind = ""
			ep, err = decide(ctx, q.Key, ep, true, next)
			if err != nil {
				return current, err
			}
		}
		reservation := store.OutputCorrectionReservation{InitialUsageID: q.InitialUsageID, ID: store.NewExecutionEpoch(), ExecutionEpoch: x.authority.Epoch, ScopeKey: q.ScopeKey, StartedAt: time.Now().UTC()}
		next := ep
		next.Attempts++
		next.Reservations = append(append([]store.OutputCorrectionReservation(nil), ep.Reservations...), reservation)
		ep, err = decide(ctx, q.Key, ep, true, next)
		if err != nil {
			return current, err
		}
		target := reservation.ReceiptTarget(q.Key, ep.Generation)
		beforeOutput, beforeViolation := correctionSemanticFingerprint(current), correctionFingerprint(validation.Error())
		candidate, usage, roundErr := q.Round(x.context(ctx), current, validation)
		// Even cancellation must settle the already-paid original reservation.
		settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		receipt := store.OutputCorrectionReceipt{Usage: usage}
		var receiptErr error
		if q.Observation != nil {
			receipt.Usage = store.OutputCorrectionUsage{}
			receipt.Observation, receiptErr = q.Observation()
			if receiptErr == nil && roundErr == nil && candidate != nil && q.Validate(candidate) == nil {
				receipt.Observation.CountIteration = true
			}
		}
		if receiptErr == nil {
			_, receiptErr = x.ledger.RecordOutputCorrectionReceipt(settleCtx, x.authority.RunID, target, receipt)
		}
		if receiptErr == nil && q.ReceiptRecorded != nil {
			receiptErr = q.ReceiptRecorded(settleCtx)
		}
		cancel()
		if candidate != nil {
			current = candidate
		}
		if receiptErr != nil {
			return current, errors.Join(roundErr, fmt.Errorf("record correction receipt: %w", receiptErr))
		}
		// A receipt advances revision. Reload accounting, but never accept a
		// successor generation or grant and never replay the call on this path.
		loadCtx, loadCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		ep, exists, err = x.load(loadCtx, q.Key)
		loadCancel()
		if err != nil {
			return current, errors.Join(roundErr, err)
		}
		if !exists || ep.Generation != target.Generation {
			return current, errors.Join(roundErr, errCorrectionOwnershipLost)
		}
		next = ep
		next.LastOutputFingerprint = correctionSemanticFingerprint(current)
		if roundErr != nil || candidate == nil {
			if roundErr == nil {
				roundErr = errors.New("output correction returned no candidate")
			}
			next.Status = correctionStatusExhausted
			next.TerminalKind = "round_failed"
			next.Cause = correctionCause(roundErr)
			next.LastError = q.safeError(roundErr)
			if next.Cause != nil && next.Cause.Kind == "node_deadline" {
				next.TerminalKind = "node_deadline"
			}
			if next.Cause == nil && (errors.Is(roundErr, context.Canceled) || errors.Is(roundErr, context.DeadlineExceeded)) && ep.Attempts < q.Budget {
				next.Status = correctionStatusActive
				next.TerminalKind = ""
			}
			settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_, err = decide(settleCtx, q.Key, ep, true, next)
			cancel()
			return current, errors.Join(roundErr, err)
		}
		validation = q.Validate(current)
		if validation == nil {
			next.Status = correctionStatusSucceeded
			next.TerminalKind = ""
			next.LastError = ""
			next.LastViolationFingerprint = ""
		} else {
			next.LastViolationFingerprint = correctionFingerprint(validation.Error())
			next.LastError = q.safeError(validation)
			if next.LastOutputFingerprint == beforeOutput && next.LastViolationFingerprint == beforeViolation && (q.PendingStep == nil || !q.PendingStep()) {
				next.Status = correctionStatusUnchanged
				next.TerminalKind = "candidate_rejected"
			}
			if next.Status != correctionStatusUnchanged && next.Attempts >= q.Budget {
				next.Status = correctionStatusExhausted
				next.TerminalKind = "candidate_rejected"
			}
		}
		// An arrived valid answer is an answer, even if the parent just expired.
		settleCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		ep, err = decide(settleCtx, q.Key, ep, true, next)
		cancel()
		if err != nil {
			return current, err
		}
		if validation == nil {
			if q.Finalize != nil {
				if err = q.Finalize(ctx, current); err != nil {
					return current, err
				}
			}
			return current, nil
		}
	}
}
