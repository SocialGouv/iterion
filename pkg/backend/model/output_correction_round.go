package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	"github.com/SocialGouv/iterion/pkg/store"
)

// schemaCorrectionRound distinguishes a received answer from a failed call.
// Validation belongs to the adapter, so invalid structured candidates can
// advance a durable fingerprint/budget loop instead of exhausting on round 1.
type schemaCorrectionRound struct {
	answered bool
	result   delegate.Result
	usage    store.OutputCorrectionUsage
	err      error
}

func schemaProviderError(f backendFields, result delegate.Result) error {
	if text := textOf(result.Output); strings.Contains(text, "API Error:") {
		line := strings.SplitN(strings.TrimSpace(text), "\n", 2)[0]
		return fmt.Errorf("model: %s: the backend produced no structured output — its result is the provider's own error: %s", f.subject(), line)
	}
	return nil
}

// correctionControlCause is checked BEFORE extraction. In particular, a
// cancellation never erases an ambiguous effect or another typed stop. Reading
// the whole graph also handles an ordinary first sibling and opaque redaction.
func correctionCanceled(err error) bool {
	return errtrack.AnyCause(err, func(cause error) bool { return errors.Is(cause, context.Canceled) })
}
func correctionNodeDeadline(err error) bool {
	return errtrack.AnyCause(err, func(cause error) bool { var deadline *ErrNodeDeadline; return errors.As(cause, &deadline) })
}
func correctionInvocationStop(err error) bool {
	return errtrack.IsAmbiguousEffect(err) || correctionCanceled(err) || correctionNodeDeadline(err)
}

// auxiliaryCorrectionStop projects only invocation-stop facts. Never return a
// provider error or its wrappers: a joined auxiliary auth/window must not be
// reinterpreted as the serving node's credential failure. The extractor logs
// its original diagnostic before this boundary.
func auxiliaryCorrectionStop(ctx context.Context, cause error) error {
	var stops []error
	if errtrack.IsAmbiguousEffect(cause) {
		stops = append(stops, errtrack.ErrAmbiguousEffect)
	}
	errtrack.AnyCause(cause, func(e error) bool {
		var deadline *ErrNodeDeadline
		if !errors.As(e, &deadline) {
			return false
		}
		stops = append(stops, &ErrNodeDeadline{NodeID: deadline.NodeID, Timeout: deadline.Timeout})
		return true
	})
	if correctionCanceled(cause) {
		stops = append(stops, context.Canceled)
	}
	if ctx.Err() != nil {
		stops = append(stops, ctx.Err())
	}
	return errors.Join(stops...)
}

func correctionControlCause(err error) bool {
	if correctionInvocationStop(err) {
		return true
	}
	return errtrack.AnyCause(err, func(cause error) bool {
		var auth *delegate.ErrAuthFailed
		var window *delegate.ErrRateLimited
		return errors.As(cause, &auth) || (errors.As(cause, &window) && (window.Kind == delegate.RateLimitKindUsageWindow || window.SelfImposed))
	})
}

func (h *OutputCorrectionHandle) sessionKey(result delegate.Result) correctionSession {
	id := result.SessionID
	if id == "" {
		id = h.task.SessionID
	}
	fingerprint := result.SessionFingerprint
	if fingerprint == "" {
		fingerprint = h.task.SessionFingerprint
	}
	if fingerprint == "" && id == h.raw.SessionID {
		fingerprint = h.raw.SessionFingerprint
	}
	return correctionSession{backend: firstNonEmpty(result.BackendName, h.backendName), id: id, fingerprint: fingerprint}
}
func (h *OutputCorrectionHandle) addDelegation(result delegate.Result) store.OutputCorrectionUsage {
	usage := store.OutputCorrectionUsage{Tokens: result.Tokens, CostUSD: result.CostUSD, UnreportedCalls: result.UnreportedCalls}
	key := h.sessionKey(result)
	if result.CostIsSessionTotal && key.id != "" {
		previous := h.sessionCosts[key]
		usage.CostUSD = max(0, result.CostUSD-previous)
		h.sessionCosts[key] = max(previous, result.CostUSD)
	}
	h.addUsage(usage)
	return usage
}
func (h *OutputCorrectionHandle) addUsage(usage store.OutputCorrectionUsage) {
	h.aggregate.Tokens += usage.Tokens
	h.aggregate.CostUSD += usage.CostUSD
	h.aggregate.UnreportedCalls += usage.UnreportedCalls
}
func (h *OutputCorrectionHandle) setCandidate(raw delegate.Result) delegate.Result {
	// Latest raw continuation is never the billed aggregate or extraction bill.
	tokens, cost, unreported, duration := h.aggregate.Tokens, h.aggregate.CostUSD, h.aggregate.UnreportedCalls, h.aggregate.Duration
	h.aggregate = copyCorrectionResult(raw)
	h.aggregate.Tokens = tokens
	h.aggregate.CostUSD = cost
	h.aggregate.UnreportedCalls = unreported
	h.aggregate.Duration = duration
	h.aggregate.CostIsSessionTotal = false
	if len(h.sessionCosts) == 1 {
		for key, cost := range h.sessionCosts {
			h.aggregate.CostIsSessionTotal = key.id != "" && cost == h.aggregate.CostUSD
		}
	}
	h.aggregate.Output = copyCorrectionOutput(raw.Output)
	if h.aggregate.Output == nil {
		h.aggregate.Output = map[string]any{}
	}
	stampDelegateOutputMeta(h.aggregate.Output, h.aggregate, h.backendName)
	stampFallbackMeta(h.aggregate.Output, h.route)
	stampServingRoute(h.aggregate.Output, h.route)
	return h.aggregate
}

func (e *ClawExecutor) runSchemaCorrectionRound(ctx context.Context, h *OutputCorrectionHandle, validation error) schemaCorrectionRound {
	parent := ctx
	if !h.deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, h.deadline)
		defer cancel()
	}
	nameDeadline := func(err error) error {
		if err == nil || parent.Err() != nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return err
		}
		var already *ErrNodeDeadline
		if errors.As(err, &already) {
			return err
		}
		return &ErrNodeDeadline{NodeID: h.fields.id, Timeout: h.timeout, Observed: err}
	}
	if err := ctx.Err(); err != nil {
		return schemaCorrectionRound{err: nameDeadline(err)}
	}
	if validation == nil {
		validation = ValidateOutputWithRegistry(h.aggregate.Output, h.schema, h.registry)
	}
	if pending := h.pendingExtraction; pending != nil {
		h.pendingExtraction = nil
		recovered, spent, ok, extractionErr := e.extractStructuredCorrection(ctx, h.fields.id, &pending.task, pending.primary, pending.secondary, h.schema, h.backendName)
		usage := store.OutputCorrectionUsage{Tokens: spent.Tokens, CostUSD: spent.CostUSD, UnreportedCalls: spent.UnreportedCalls}
		h.addUsage(usage)
		if ok {
			return schemaCorrectionRound{answered: true, result: h.setCandidate(recovered), usage: usage}
		}
		// An auxiliary extraction failure does not change the serving node's
		// credential identity or authorize generic replay of its business task.
		// Cancellation/deadline still stop the invocation after its bill is kept.
		stop := auxiliaryCorrectionStop(ctx, extractionErr)
		if ctx.Err() == nil && correctionCanceled(stop) && !errtrack.IsAmbiguousEffect(stop) && !correctionNodeDeadline(stop) {
			return schemaCorrectionRound{usage: usage, err: &OutputSchemaError{Stopped: true, Cause: stop}}
		}
		if stop != nil {
			return schemaCorrectionRound{usage: usage, err: nameDeadline(stop)}
		}

		return schemaCorrectionRound{answered: true, result: h.setCandidate(h.raw), usage: usage}
	}
	// Copy again before every backend crossing: a backend can retain and mutate
	// its task data without changing the frozen restart prompt or sibling handle.
	task, copyErr := copyCorrectionTask(h.task)
	if copyErr != nil {
		return schemaCorrectionRound{err: copyErr}
	}
	first := copyCorrectionResult(h.raw)
	feedbackValidation := validation
	if first.ParseFallback {
		feedbackValidation = errors.Join(validateStructuredResult(first, h.schema), validation)
	}
	reask := e.planSchemaReask(context.WithValue(ctx, correctionConversationKey{}, h.continuation), h.backendName, &task, first, formatSchemaRetryFeedback(feedbackValidation))
	if e.logger != nil {
		e.logger.Warn("[%s#%d/%s] structured output validation failed, re-asking the model (%s): %v", h.fields.id, task.Iteration, h.backendName, reask.label(), validation)
	}
	if e.hooks.OnDelegateRetry != nil {
		di := delegateInfoFromResult(h.backendName, first)
		di.DeclaredModel = task.Model
		di.Error = validation
		di.Attempt = 1
		di.Reask = reask.mode
		e.hooks.OnDelegateRetry(h.fields.id, di)
	}
	if e.hooks.OnDelegateStarted != nil {
		e.hooks.OnDelegateStarted(h.fields.id, DelegateInfo{BackendName: h.backendName, DeclaredModel: task.Model, Attempt: 2, Reask: reask.mode, ReaskInChain: h.fields.inChain})
	}
	result, callErr := e.retryDelegateLoopReask(ctx, h.fields.id, h.backendName, sharesSession(&reask.task), reask.mode, func() (delegate.Result, error) {
		task, err := copyCorrectionTask(reask.task)
		if err != nil {
			return delegate.Result{}, err
		}
		return e.executeDelegate(withExecutionUsageBackend(ctx, h.backendName), h.backend, task)
	})
	usage := h.addDelegation(result)
	// Event spend uses the same session high-water as the receipt, not a raw
	// cumulative value (which may be lower than an earlier reported maximum).
	eventResult := result
	eventResult.CostUSD = usage.CostUSD
	eventResult.CostIsSessionTotal = false
	e.emitReaskOutcome(h.fields.id, h.backendName, task.Model, reask, eventResult, callErr, usage.CostUSD, 2)
	h.raw = copyCorrectionResult(result)
	if result.SessionID == "" {
		h.raw.SessionID = first.SessionID
	}
	if result.SessionFingerprint == "" {
		h.raw.SessionFingerprint = first.SessionFingerprint
	}
	if h.backendName == delegate.BackendClaw {
		h.continuation = append(json.RawMessage(nil), e.completedClawConversation(ctx, &h.task, h.raw)...)
	}
	h.aggregate.Duration += result.Duration
	callErr = nameDeadline(callErr)
	if callErr != nil && correctionControlCause(callErr) {
		h.setCandidate(h.raw)
		return schemaCorrectionRound{usage: usage, err: schemaReaskFailure(h.fields.subject(), validation, reask, callErr)}
	}
	if err := schemaProviderError(h.fields, result); err != nil && callErr == nil {
		h.setCandidate(h.raw)
		return schemaCorrectionRound{usage: usage, err: err}
	}
	if callErr != nil || result.ParseFallback {
		if err := ctx.Err(); err != nil {
			h.setCandidate(h.raw)
			return schemaCorrectionRound{usage: usage, err: schemaReaskFailure(h.fields.subject(), validation, reask, nameDeadline(errors.Join(callErr, err)))}
		}
		if strings.TrimSpace(fallbackText(result.Output)) != "" || strings.TrimSpace(fallbackText(first.Output)) != "" {
			h.pendingExtraction = &correctionExtractionStep{task: task, primary: copyCorrectionResult(result), secondary: copyCorrectionResult(first)}
			return schemaCorrectionRound{answered: true, result: h.setCandidate(h.raw), usage: usage}
		}
		if result.ParseFallback {
			return schemaCorrectionRound{answered: true, result: h.setCandidate(h.raw), usage: usage}
		}
		h.setCandidate(h.raw)
		return schemaCorrectionRound{usage: usage, err: schemaReaskFailure(h.fields.subject(), validation, reask, callErr)}
	}

	// A response already received remains Answered even if ctx expired as it
	// arrived. Its owner controls validation/commit under its execution fence.
	if err := schemaProviderError(h.fields, result); err != nil {
		h.setCandidate(h.raw)
		return schemaCorrectionRound{usage: usage, err: err}
	}
	return schemaCorrectionRound{answered: true, result: h.setCandidate(h.raw), usage: usage}
}
