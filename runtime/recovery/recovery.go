// Package recovery defines typed recovery recipes that decide what to
// do when a node fails. The runtime engine consults a recipe per
// error class to choose between retrying the same node, forcing a
// compaction, pausing for human intervention, or failing terminally.
//
// The package is deliberately decoupled from the engine: a Recipe
// returns an Action describing what to do, leaving the actual
// execution (retry, compact, pause) to the caller.
//
// STATUS — engine wiring deferred. The Recipe / Action / Classify
// surface is stable and unit-tested, but `runtime/engine.go` does
// NOT yet consult these recipes when `executor.Execute` returns an
// error — it currently falls straight through to
// `failRunWithCheckpoint`. Wiring is deferred because:
//
//   - ActionCompactAndRetry needs an executor-level Compact() hook
//     (claw exposes ConversationLoop.Compact but the
//     `runtime.NodeExecutor` abstraction does not surface it).
//   - ActionPauseForHuman needs a synthetic interaction record
//     compatible with `iterion resume --answers-file`.
//   - ActionRetrySameNode needs the engine to track per-node
//     attempt counts across the loop.
//
// New code that produces ErrCodeRateLimited / ErrCodeContextLengthExceeded
// / ErrCodeToolFailedTransient / ErrCodeToolFailedPermanent should
// continue to do so; the recipes will start consuming them once the
// engine is wired. See TODO(recovery) markers in runtime/engine.go.
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

// ActionKind enumerates the recipes' possible decisions.
type ActionKind int

const (
	// ActionRetrySameNode means: re-execute the failing node from
	// scratch (or from its checkpoint if available). Use Delay to
	// throttle the retry; AttemptsLeft tracks the budget.
	ActionRetrySameNode ActionKind = iota

	// ActionCompactAndRetry means: ask the LLM client to drop older
	// turns from the conversation history (claw exposes
	// ConversationLoop.Compact for this) and then retry.
	ActionCompactAndRetry

	// ActionPauseForHuman means: pause the run with a human
	// interaction so an operator can decide (extend budget, change
	// auth, fix a tool definition) before resuming.
	ActionPauseForHuman

	// ActionFailTerminal means: surface the error to the caller as a
	// non-resumable failure. Used for permanent tool failures
	// (missing files, invalid config) that cannot be recovered.
	ActionFailTerminal
)

// Action carries a recipe's decision plus parameters relevant to that
// kind. Zero values are safe (treated as ActionRetrySameNode with no
// delay).
type Action struct {
	Kind         ActionKind
	Delay        time.Duration // for ActionRetrySameNode / ActionCompactAndRetry
	AttemptsLeft int           // 0 means "no more retries; escalate"
	Reason       string        // human-readable hint surfaced to operator
}

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
