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

	"github.com/SocialGouv/iterion/runtime"
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

// ContextLengthRecipe: compact-and-retry once, then fail terminal
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

// DefaultRecipes maps each well-known error code to its default
// recipe. Hosts can override individual entries before installing.
func DefaultRecipes() map[runtime.ErrorCode]Recipe {
	return map[runtime.ErrorCode]Recipe{
		runtime.ErrCodeRateLimited:           RateLimitRecipe(DefaultMaxRetries),
		runtime.ErrCodeContextLengthExceeded: ContextLengthRecipe(),
		runtime.ErrCodeBudgetExceeded:        BudgetRecipe(),
		runtime.ErrCodeToolFailedTransient:   TransientToolRecipe(2),
		runtime.ErrCodeToolFailedPermanent:   PermanentToolRecipe(),
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
	var apiErr *api.APIError
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode == 429 {
			return runtime.ErrCodeRateLimited
		}
		body := strings.ToLower(apiErr.Message)
		if strings.Contains(body, "context_length_exceeded") || strings.Contains(body, "context length") {
			return runtime.ErrCodeContextLengthExceeded
		}
		return runtime.ErrCodeExecutionFailed
	}
	return runtime.ErrCodeExecutionFailed
}
