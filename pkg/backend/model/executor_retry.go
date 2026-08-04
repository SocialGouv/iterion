package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// ---------------------------------------------------------------------------
// Retry classifiers + the delegate-retry loop.
//
// Carved out of executor.go to keep the file's bulk focused on Execute
// flow control. Lives in the same package so the helpers stay private.
// ---------------------------------------------------------------------------

// isRetryable returns true if err is a transient LLM API error that should be
// retried. Recognises both iterion's local *APIError (used for stream-decoded
// errors) and claw-code-go's *clawapi.APIError (returned by provider HTTP
// clients on non-2xx responses, e.g. 429 / 5xx).
func isRetryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.IsRetryable
	}
	var clawErr *api.APIError
	if errors.As(err, &clawErr) {
		return clawErr.IsRetryable()
	}
	// Raw transport failures the typed API errors don't cover: a mid-call
	// connection reset / DNS flap / i/o timeout reaches the in-process claw
	// backend as a bare net error, not an *APIError.
	return delegate.IsNetworkError(err)
}

// statusCodeOf extracts the HTTP status code from a recognised API error
// type, or 0 when the error is not an API error.
func statusCodeOf(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	var clawErr *api.APIError
	if errors.As(err, &clawErr) {
		return clawErr.StatusCode
	}
	return 0
}

// isDelegateRetryable determines whether a backend execution error is transient
// and worth retrying. Only signal-based kills (exit codes 128+, indicating
// OOM, SIGTERM, etc.) and I/O errors are retried. Permanent failures like
// exit 1 (application error), exit 2 (misuse), or exit 127 (command not
// found) are not retried.
//
// Typed-error fast paths come first: an *ErrTransient or *ErrRateLimited
// raised explicitly by a backend bypasses the legacy stderr-string
// heuristics, which are kept as a fallback for backends that haven't
// been migrated yet (and for SDK-internal errors we don't own).
func isDelegateRetryable(err error) bool {
	if err == nil {
		return false
	}
	var transient *delegate.ErrTransient
	if errors.As(err, &transient) {
		return true
	}
	var rateLimited *delegate.ErrRateLimited
	if errors.As(err, &rateLimited) {
		return true
	}
	// Transient connectivity failure (DNS / TCP / TLS / upstream 5xx) — a
	// net.Error, a wrapped syscall errno, or a stringified marker bubbled up
	// from a CLI delegate ("fetch failed", "ECONNRESET", "overloaded"). A
	// brief internet outage must not abort a whole multi-node run.
	if delegate.IsNetworkError(err) {
		return true
	}
	msg := err.Error()
	// Subprocess killed by signal (OOM, timeout, etc.).
	if strings.Contains(msg, "signal:") {
		return true
	}
	// Exit status: only retry signal-based exits (128+). Lower exit codes
	// indicate permanent failures that retrying won't fix.
	if strings.Contains(msg, "exit status") {
		code := extractExitCode(msg)
		// exit 128+ means the process was killed by a signal (128+N).
		// These are typically transient (OOM killer, timeout, etc.).
		return code >= 128
	}
	// Process could not start (resource exhaustion).
	if strings.Contains(msg, "failed to start") {
		return true
	}
	// Stdout reading failure (broken pipe, etc.).
	if strings.Contains(msg, "reading stdout") {
		return true
	}
	// claude_code SDK fell silent for too long (we observed sessions
	// hanging in ep_poll without any propagated error). The runSession
	// watchdog aborts and surfaces this — retrying usually picks up
	// where the previous attempt left off because the resumed session
	// gets a fresh subprocess.
	if strings.Contains(msg, "session idle for") {
		return true
	}
	return false
}

// extractExitCode parses an exit code from an error message containing
// "exit status N". Returns -1 if no valid code is found.
func extractExitCode(msg string) int {
	const prefix = "exit status "
	idx := strings.Index(msg, prefix)
	if idx < 0 {
		return -1
	}
	rest := msg[idx+len(prefix):]
	// Parse the integer, stopping at first non-digit.
	n := 0
	found := false
	for _, c := range rest {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
			found = true
		} else {
			break
		}
	}
	if !found {
		return -1
	}
	return n
}

// retryReason renders a short human label for the retry log, distinguishing
// a connectivity blip from other transient failures (OOM signal, idle hang)
// so the operator can tell "internet hiccup" from "subprocess died".
func retryReason(err error) string {
	if delegate.IsNetworkError(err) {
		return "network connectivity issue"
	}
	return "transient backend error"
}

// shouldRetryInPlace decides whether a failed delegate call is worth
// re-issuing against the SAME chain element. It is isDelegateRetryable
// with one carve-out: a provider usage WINDOW (the subscription 5h or
// weekly cap) cannot be cured by waiting a few seconds, so when the node
// still has a fallback element left to try, the in-place budget is
// skipped entirely and the chain advances immediately.
//
// Without the carve-out every element pays a full backed-off budget
// inside a window where success is impossible by construction. That is
// not merely slow: the whole chain runs under ONE per-node timeout
// context, so a multi-element chain can hit the node deadline mid-walk
// and surface context.DeadlineExceeded instead of the typed
// *ErrRateLimited that the run-level usage-window retry and the
// credential-pool donor cooldown both key on.
//
// On the LAST element the carve-out does not apply: with nowhere better
// to go, today's behaviour (retry, then surface the typed error upward)
// is exactly right.
func shouldRetryInPlace(err error, fallbackRemains bool) bool {
	if fallbackRemains && delegate.IsUsageWindow(err) {
		return false
	}
	return isDelegateRetryable(err)
}

// retryDelegateLoop retries a backend execution call with exponential backoff.
// The attempt budget is error-adaptive: a network/connectivity failure gets
// the larger transient budget (rides out a multi-second outage), while a
// deterministic-but-retryable error (signal kill, idle hang) keeps the
// standard budget.
//
// This is the no-fallback form: callers with no further chain element
// (the schema-validation retry, direct one-shot dispatch) use it and get
// the historical behaviour unchanged.
func (e *ClawExecutor) retryDelegateLoop(ctx context.Context, nodeID string, backendName string, fn func() (delegate.Result, error)) (delegate.Result, error) {
	return e.retryDelegateLoopChain(ctx, nodeID, backendName, false, fn)
}

// retryDelegateLoopChain is retryDelegateLoop with knowledge of whether
// the caller still has a fallback element to fall through to, which lets
// it skip a budget that cannot succeed (see shouldRetryInPlace).
func (e *ClawExecutor) retryDelegateLoopChain(ctx context.Context, nodeID string, backendName string, fallbackRemains bool, fn func() (delegate.Result, error)) (delegate.Result, error) {
	result, err := fn()
	for attempt := 1; err != nil && shouldRetryInPlace(err, fallbackRemains); attempt++ {
		maxAttempts := e.retry.effectiveMaxAttempts(err)
		if attempt >= maxAttempts {
			break
		}
		delay := e.retry.backoff(attempt - 1)

		e.logger.Warn("[%s#%d/%s] %s — delegate retry %d/%d after error: %v (backoff %s)",
			nodeID, LoopIterationFromContext(ctx), backendName, retryReason(err), attempt, maxAttempts-1, err, delay.Round(time.Millisecond))

		if e.hooks.OnDelegateRetry != nil {
			e.hooks.OnDelegateRetry(nodeID, DelegateInfo{
				BackendName: backendName,
				Attempt:     attempt,
				Error:       err,
				Delay:       delay,
			})
		}

		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return delegate.Result{}, ctx.Err()
		}

		result, err = fn()
	}
	return result, err
}

// ---------------------------------------------------------------------------
// Provider fallback chain.
//
// Generalises the single-node RESCUE_PROVIDER escape hatch into a
// declarative, ordered chain (DSL `provider: "anthropic,zai,openai"`).
// The chain wraps retryDelegateLoop: each provider gets the full retry
// budget; only a hard failure *beyond* that budget falls through to the
// next provider. See docs/adr for the credential-hint-vs-cross-model
// scope decision.
// ---------------------------------------------------------------------------

// providerLabel renders a provider hint for logs, mapping the empty
// "auto" hint to a readable token.
func providerLabel(p string) string {
	if p == "" {
		return "auto"
	}
	return p
}

// stepLabel renders a chain element for logs: "zai" when it inherits the
// node model, "zai/glm-5.2" when it pins its own. The empty "auto" hint
// maps to a readable token.
func stepLabel(s providerStep) string {
	if s.Model == "" {
		return providerLabel(s.Provider)
	}
	return providerLabel(s.Provider) + "/" + s.Model
}

// chainLabel renders a whole chain in its `provider:model` source form
// for the exhausted-chain error, e.g. "zai:glm-5.2,anthropic:claude-opus-4-8".
func chainLabel(chain []providerStep) string {
	parts := make([]string, len(chain))
	for i, s := range chain {
		p := providerLabel(s.Provider)
		if s.Model != "" {
			p += ":" + s.Model
		}
		parts[i] = p
	}
	return strings.Join(parts, ",")
}

// providerFallbackEligible reports whether a backend actually consumes
// the per-node provider hint, and therefore whether walking a
// multi-element provider chain is meaningful. Only claude_code honours
// ProviderHint today (anthropic ↔ z.ai ↔ Anthropic-compatible facades);
// claw derives its provider from the model-spec prefix and codex ignores
// the hint entirely, so for those a multi-provider chain would re-run an
// identical call and waste a second retry budget. Compile-time C088
// warns the author; here we collapse the chain to its head so the run
// never pays for a no-op fall-through.
//
// Centralised + named so wiring a future hint-honouring backend (e.g.
// teaching claw to switch provider+model per element) is a one-line
// change.
func providerFallbackEligible(backendName string) bool {
	return backendName == delegate.BackendClaudeCode
}

// dispatchWithProviderFallback runs backend.Execute across the node's
// provider chain, transparently falling through to the next provider on
// a hard failure beyond the retry budget. It mutates task.ProviderHint
// (and, for elements that pin one, task.Model) per attempt and returns
// the first success, or the last error once the chain is exhausted.
//
// "Hard failure" is any non-nil error returned by retryDelegateLoop —
// a non-retryable error, or a retryable one that exhausted the budget.
// Context cancellation / deadline is NOT a provider failure: it aborts
// the chain immediately so a cancelled run doesn't thrash through every
// provider. Each fall-through emits exactly one log note and one
// OnProviderFallback hook, so the operator sees a route change, not a
// failure.
//
// task.Model as built by buildTask is the node baseline. An element that
// declares a `provider:model` model overrides it for that attempt; an
// element without one (Model == "") restores the baseline — so a
// model-less element after a model-bearing one does NOT inherit the
// previous element's override.
func (e *ClawExecutor) dispatchWithProviderFallback(
	ctx context.Context,
	nodeID, backendName string,
	chain []providerStep,
	backend delegate.Backend,
	task *delegate.Task,
) (delegate.Result, error) {
	if len(chain) == 0 {
		chain = []providerStep{{}}
	}
	// Backends that ignore the provider hint gain nothing from walking
	// the chain — collapse to the preferred provider to avoid a wasted
	// second retry budget on an identical call.
	if len(chain) > 1 && !providerFallbackEligible(backendName) {
		chain = chain[:1]
	}

	baseModel := task.Model
	effectiveModel := func(s providerStep) string {
		if s.Model != "" {
			return s.Model
		}
		return baseModel
	}

	var (
		result delegate.Result
		err    error
	)
	for i, step := range chain {
		task.ProviderHint = step.Provider
		task.Model = effectiveModel(step)
		fallbackRemains := i < len(chain)-1
		result, err = e.retryDelegateLoopChain(ctx, nodeID, backendName, fallbackRemains, func() (delegate.Result, error) {
			return backend.Execute(ctx, *task)
		})
		if err == nil {
			return result, nil
		}
		// A cancelled / timed-out context is terminal for the whole
		// node, not a provider-specific failure: don't fall through.
		if ctx.Err() != nil {
			return result, err
		}
		if i < len(chain)-1 {
			next := chain[i+1]
			if e.logger != nil {
				e.logger.Warn("[%s#%d/%s] provider %q failed beyond retry budget; falling through to %q: %v",
					nodeID, LoopIterationFromContext(ctx), backendName,
					stepLabel(step), stepLabel(next), err)
			}
			if e.hooks.OnProviderFallback != nil {
				e.hooks.OnProviderFallback(nodeID, ProviderFallbackInfo{
					BackendName: backendName,
					From:        step.Provider,
					To:          next.Provider,
					FromModel:   effectiveModel(step),
					ToModel:     effectiveModel(next),
					// A legacy `provider:` chain never changes backend;
					// both sides are the node's resolved one. The fields
					// exist so the emitted event has its final shape
					// before `fallbacks:` starts varying them.
					FromBackend: backendName,
					ToBackend:   backendName,
					Reason:      string(delegate.ClassifyFallback(err, isDelegateRetryable(err))),
					Attempts:    e.retry.maxAttempts(),
					Err:         err,
				})
			}
		}
	}
	// Whole chain exhausted. Annotate with the chain when it had real
	// alternatives so the surfaced error explains the multi-provider
	// attempt; a single-element chain keeps the bare backend error.
	if len(chain) > 1 {
		return result, fmt.Errorf("all providers in chain %s failed; last error: %w", chainLabel(chain), err)
	}
	return result, err
}
