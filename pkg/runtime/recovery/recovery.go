// Package recovery defines typed recovery recipes that decide what to
// do when a node fails. The runtime engine consults a recipe per
// error class to choose between retrying the same node, forcing a
// compaction, pausing for human intervention, or failing terminally.
//
// The package is deliberately decoupled from the engine: a Recipe
// returns an Action describing what to do, leaving the actual
// execution (retry, compact, pause) to the caller. Hosts wire the
// dispatcher into the engine via runtime.WithRecoveryDispatch and
// recovery.Dispatch(DefaultRecipes()).
package recovery

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// ActionKind is an alias for the engine-facing decision kind so existing
// callers of the recovery package keep working after the engine wiring.
type ActionKind = runtime.RecoveryActionKind

const (
	// ActionRetrySameNode re-executes the failing node, optionally
	// after Delay; AttemptsLeft tracks the remaining budget.
	ActionRetrySameNode = runtime.RecoveryRetrySameNode

	// ActionCompactAndRetry asks the LLM client to drop older
	// conversation turns (when supported) and then retry. Falls back
	// to a plain retry when the executor doesn't implement Compactor.
	ActionCompactAndRetry = runtime.RecoveryCompactAndRetry

	// ActionPauseForHuman pauses the run with a synthetic
	// interaction so an operator can resolve and resume.
	ActionPauseForHuman = runtime.RecoveryPauseForHuman

	// ActionFailTerminal surfaces the error as a non-recoverable
	// failure (still produces a checkpoint via failRunWithCheckpoint).
	ActionFailTerminal = runtime.RecoveryFailTerminal
)

// Action is the engine-facing decision returned by a recipe.
type Action = runtime.RecoveryAction

// Recipe decides what to do for a given error class.
// `attempts` is the count of prior retries for this class on this
// node (zero on first failure).
type Recipe interface {
	Apply(ctx context.Context, err *runtime.RuntimeError, attempts int) Action
}

type RecipeFunc func(ctx context.Context, err *runtime.RuntimeError, attempts int) Action

func (f RecipeFunc) Apply(ctx context.Context, err *runtime.RuntimeError, attempts int) Action {
	return f(ctx, err, attempts)
}

// DefaultMaxRetries is the per-class retry budget unless overridden
// per recipe.
const DefaultMaxRetries = 3

// RateLimitRecipe: exponential backoff + jitter, escalates to human
// pause after maxRetries (operator may rotate credentials).
func RateLimitRecipe(maxRetries int) Recipe {
	if maxRetries <= 0 {
		maxRetries = DefaultMaxRetries
	}
	return RecipeFunc(func(_ context.Context, err *runtime.RuntimeError, attempts int) Action {
		if attempts >= maxRetries {
			return Action{
				Kind:   ActionPauseForHuman,
				Reason: "rate limit retries exhausted; operator should wait for quota reset or rotate credentials",
			}
		}
		// Exponential backoff: 4s, 8s, 16s, 32s capped, with ±25% jitter.
		base := time.Duration(math.Pow(2, float64(attempts+2))) * time.Second
		if base > 32*time.Second {
			base = 32 * time.Second
		}
		jitter := time.Duration(rand.Int64N(int64(base / 2)))
		return Action{
			Kind:         ActionRetrySameNode,
			Delay:        base/2 + jitter,
			AttemptsLeft: maxRetries - attempts - 1,
			Reason:       "rate limited; backing off",
		}
	})
}

// ContextLengthRecipe: compact-and-retry twice, then fail terminal
// (the conversation can't be made smaller).
func ContextLengthRecipe() Recipe {
	return RecipeFunc(func(_ context.Context, err *runtime.RuntimeError, attempts int) Action {
		if attempts >= 2 {
			return Action{
				Kind:   ActionFailTerminal,
				Reason: "compaction did not reduce context enough to fit the model window",
			}
		}
		return Action{
			Kind:   ActionCompactAndRetry,
			Delay:  100 * time.Millisecond,
			Reason: "context length exceeded; compacting older turns and retrying",
		}
	})
}

// BudgetRecipe: always pause for human. There is no automatic retry
// path for budget exhaustion — operator must extend or terminate.
func BudgetRecipe() Recipe {
	return RecipeFunc(func(_ context.Context, _ *runtime.RuntimeError, _ int) Action {
		return Action{
			Kind:   ActionPauseForHuman,
			Reason: "budget exhausted; extend the budget via .iter file or terminate the run",
		}
	})
}

// TransientToolRecipe: retry with linear backoff up to maxRetries
// (model gets the error in its next turn), then fail terminal.
func TransientToolRecipe(maxRetries int) Recipe {
	if maxRetries <= 0 {
		maxRetries = 2
	}
	return RecipeFunc(func(_ context.Context, _ *runtime.RuntimeError, attempts int) Action {
		if attempts >= maxRetries {
			return Action{
				Kind:   ActionFailTerminal,
				Reason: "transient tool failure persisted past retry budget; surfacing to operator",
			}
		}
		return Action{
			Kind:         ActionRetrySameNode,
			Delay:        time.Duration(attempts+1) * time.Second,
			AttemptsLeft: maxRetries - attempts - 1,
			Reason:       "transient tool failure; retrying after short delay",
		}
	})
}

// PermanentToolRecipe: no retry, immediately escalate to terminal.
func PermanentToolRecipe() Recipe {
	return RecipeFunc(func(_ context.Context, err *runtime.RuntimeError, _ int) Action {
		return Action{
			Kind:   ActionFailTerminal,
			Reason: "tool failure is permanent (e.g. missing file, invalid arguments); not retrying",
		}
	})
}

// ExecutionFailedRecipe: the catch-all bucket for unclassified node
// failures. Tries one retry with a short delay (covers transient
// subprocess crashes, flaky network blips, momentary fs races), then
// falls through to FailTerminal so the engine produces a
// failed_resumable checkpoint that the operator can /resume after
// fixing the root cause. Without this recipe registered, the first
// failure of any unclassified node short-circuits to FailTerminal
// with no retry attempted at all.
func ExecutionFailedRecipe(maxRetries int) Recipe {
	if maxRetries <= 0 {
		maxRetries = 1
	}
	return RecipeFunc(func(_ context.Context, _ *runtime.RuntimeError, attempts int) Action {
		if attempts >= maxRetries {
			return Action{
				Kind:   ActionFailTerminal,
				Reason: "node execution kept failing after retries; surfacing as failed_resumable so the operator can /resume after fixing the cause",
			}
		}
		return Action{
			Kind:         ActionRetrySameNode,
			Delay:        2 * time.Second,
			AttemptsLeft: maxRetries - attempts - 1,
			Reason:       "node execution failed; retrying once before surfacing as failed_resumable",
		}
	})
}

// NetworkTransientRecipe: longer exponential-backoff loop for transient
// network failures reaching the upstream model API (ISP blip, captive
// portal handoff, DNS flutter, datacenter routing change). Each attempt
// doubles the delay up to a 60s cap; with the default 6 retries that
// covers ~10 min of cumulative wait — enough to ride out the kind of
// outages an operator would expect a long-running pipeline to recover
// from on its own. Beyond that we surface as failed_resumable so the
// operator can /resume after the network is verified back.
//
// Why a separate recipe (not just a bigger ExecutionFailedRecipe):
// non-network execution failures (schema mismatch, missing fs entry,
// in-sandbox script crash) usually won't fix themselves on retry —
// burning 6 * 60s on a deterministic bug wastes operator time. Network
// transients DO routinely fix themselves; rewarding the right pattern
// matters for unattended overnight runs.
//
// Default cap = 6 attempts, 60s max backoff. Hosts override via
// DefaultRecipes map mutation if they want a different shape.
func NetworkTransientRecipe(maxRetries int) Recipe {
	if maxRetries <= 0 {
		maxRetries = 6
	}
	const baseDelay = 5 * time.Second
	const maxDelay = 60 * time.Second
	return RecipeFunc(func(_ context.Context, _ *runtime.RuntimeError, attempts int) Action {
		if attempts >= maxRetries {
			return Action{
				Kind:   ActionFailTerminal,
				Reason: "network outage exceeded retry budget; surfacing as failed_resumable — verify connectivity, then /resume",
			}
		}
		// Exponential backoff capped at maxDelay: 5, 10, 20, 40, 60, 60.
		// Short-circuit when the desired delay is bound to exceed
		// maxDelay anyway — avoids needing to reason about whether
		// `baseDelay * (1 << shift)` over- or underflowed before the
		// `delay > maxDelay` clamp can catch it. For 5s base, shift=4
		// already produces 80s which is well beyond the 60s cap.
		if attempts >= 4 {
			return Action{
				Kind:         ActionRetrySameNode,
				Delay:        maxDelay,
				AttemptsLeft: maxRetries - attempts - 1,
				Reason:       "transient network failure; retrying with exponential backoff",
			}
		}
		shift := uint(attempts)
		delay := baseDelay * (1 << shift)
		if delay > maxDelay || delay <= 0 {
			delay = maxDelay
		}
		return Action{
			Kind:         ActionRetrySameNode,
			Delay:        delay,
			AttemptsLeft: maxRetries - attempts - 1,
			Reason:       "transient network failure; retrying with exponential backoff",
		}
	})
}

// DefaultRecipes maps each well-known error code to its default
// recipe. Hosts can override individual entries before installing.
func DefaultRecipes() map[runtime.ErrorCode]Recipe {
	return map[runtime.ErrorCode]Recipe{
		runtime.ErrCodeRateLimited:           RateLimitRecipe(DefaultMaxRetries),
		runtime.ErrCodeContextLengthExceeded: ContextLengthRecipe(),
		runtime.ErrCodeBudgetExceeded:        BudgetRecipe(),
		runtime.ErrCodeToolFailedTransient:   TransientToolRecipe(2),
		runtime.ErrCodeToolFailedPermanent:   PermanentToolRecipe(),
		runtime.ErrCodeExecutionFailed:       ExecutionFailedRecipe(1),
		runtime.ErrCodeNetworkTransient:      NetworkTransientRecipe(6),
	}
}

// Dispatch turns a recipe map into a runtime.RecoveryDispatch
// callback. The dispatcher classifies, asks the engine for the prior
// attempt count under that class, and returns the recipe's decision
// plus the matched code. Errors that don't classify into a wired code
// fall through to RecoveryFailTerminal.
//
// Safe for concurrent use as long as the wrapped recipes are.
func Dispatch(recipes map[runtime.ErrorCode]Recipe) runtime.RecoveryDispatch {
	return func(ctx context.Context, err error, priorAttempts func(runtime.ErrorCode) int) (runtime.RecoveryAction, runtime.ErrorCode) {
		if err == nil {
			return runtime.RecoveryAction{Kind: runtime.RecoveryFailTerminal}, ""
		}
		code := Classify(err)
		recipe, ok := recipes[code]
		if !ok {
			return runtime.RecoveryAction{
				Kind:   runtime.RecoveryFailTerminal,
				Reason: "no recipe registered for error code " + string(code),
			}, code
		}
		var rerr *runtime.RuntimeError
		if !errors.As(err, &rerr) {
			rerr = &runtime.RuntimeError{Code: code, Message: err.Error(), Cause: err}
		}
		attempts := 0
		if priorAttempts != nil {
			attempts = priorAttempts(code)
		}
		return recipe.Apply(ctx, rerr, attempts), code
	}
}

// Classify inspects err and returns the canonical RuntimeError code
// for it. It recognises:
//
//   - *runtime.RuntimeError → its declared Code
//   - *delegate.ErrRateLimited → RATE_LIMITED (CLI-backend rate-limit
//     signal raised when the assistant text matches the provider's
//     quota wording; not an api.APIError because the CLI's wire is
//     pre-parsed JSON, so the 429 never reaches the SDK as such)
//   - *api.APIError with StatusCode 429 → RATE_LIMITED
//   - *api.APIError with body containing "context_length_exceeded"
//     or "context length" → CONTEXT_LENGTH_EXCEEDED
//   - any other *api.APIError → EXECUTION_FAILED
//
// Hosts that want richer classification can wrap or replace this
// function.
func Classify(err error) runtime.ErrorCode {
	if err == nil {
		return ""
	}
	var rerr *runtime.RuntimeError
	if errors.As(err, &rerr) {
		return rerr.Code
	}
	var rateLimited *delegate.ErrRateLimited
	if errors.As(err, &rateLimited) {
		return runtime.ErrCodeRateLimited
	}
	var apiErr *api.APIError
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode == 429 {
			return runtime.ErrCodeRateLimited
		}
		body := strings.ToLower(apiErr.Message)
		if strings.Contains(body, "context_length_exceeded") || strings.Contains(body, "context length") {
			return runtime.ErrCodeContextLengthExceeded
		}
		// 5xx and 408 (Request Timeout) from the upstream API are
		// textbook transient failures: gateway timeouts, upstream
		// connection resets, service overload, deploys mid-rollout.
		// Route them to NETWORK_TRANSIENT so the longer exponential
		// backoff recipe (~10 min) absorbs the outage instead of the
		// catch-all 1-shot ExecutionFailed retry. 409 is intentionally
		// excluded — it's usually a logical conflict, not a transient
		// network issue.
		if apiErr.StatusCode == 408 || (apiErr.StatusCode >= 500 && apiErr.StatusCode <= 599) {
			return runtime.ErrCodeNetworkTransient
		}
		return runtime.ErrCodeExecutionFailed
	}
	// String-pattern fallback for unstructured errors that bubble up
	// from out-of-process backends (claude_code CLI, iterion sandbox
	// drivers). The Anthropic CLI emits "API Error: Unable to connect
	// to API (FailedToOpenSocket)" when the host loses the socket
	// mid-request; OpenAI tools emit variations on "connection refused"
	// / "connection reset"; Go's net/http surface prints "no such host"
	// for DNS failures + "i/o timeout" for read/dial timeouts. None of
	// these arrive as *api.APIError because the upstream never produced
	// an HTTP response; the wrapping layers stringify the lower-level
	// error and we don't have a typed channel for it. Match the known
	// phrases case-insensitively and route to NETWORK_TRANSIENT so the
	// recovery dispatcher applies the longer exponential backoff
	// instead of the catch-all 1-shot retry.
	msg := strings.ToLower(err.Error())
	for _, needle := range networkTransientNeedles {
		if strings.Contains(msg, needle) {
			return runtime.ErrCodeNetworkTransient
		}
	}
	return runtime.ErrCodeExecutionFailed
}

// networkTransientNeedles enumerates the lowercase substrings that
// indicate a transient connectivity failure to the upstream model API.
// Kept as a plain slice so hosts patching this file can append site-
// specific phrases (corporate proxy timeouts, etc.) without touching
// the matcher logic. ALL entries MUST be lowercase — Classify lowercases
// the err string before checking.
var networkTransientNeedles = []string{
	"failedtoopensocket",        // anthropic claude CLI verbatim ("Unable to connect to API (FailedToOpenSocket)")
	"unable to connect to api",  // anthropic claude CLI prefix
	"connection refused",        // tcp refused (host present, port closed / firewall)
	"connection reset",          // tcp reset by peer (mid-request drop)
	"no such host",              // dns failure (transient resolver outage)
	"i/o timeout",               // go net package timeouts (dial / read / write)
	"context deadline exceeded", // request-level timeout in upstream client
	"network is unreachable",    // host has no route (eg vpn dropped)
	"no route to host",          // routing-table drop (often transient)
	"tls handshake timeout",     // tls negotiation hung (proxy / mitm flap)
	"unexpected eof",            // SSE / streaming response cut short by peer
	"http: eof",                 // net/http verbatim: server hung up mid-stream
	"http2: timeout",            // golang.org/x/net/http2: "http2: timeout awaiting response headers"
	"http2: server sent goaway", // http/2 graceful shutdown signal from upstream mid-request
	"upstream connect error",    // envoy / cloudfront gateway: e.g. "upstream connect error or disconnect/reset before headers"
	"server closed idle connection",
}
