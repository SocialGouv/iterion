package delegate

import (
	"errors"
	"time"
)

// ---------------------------------------------------------------------------
// Fallback-trigger classification.
//
// The canonical mapping from a backend execution error to the coarse
// category a fallback chain can filter on (`fallbacks: … on: […]`, see
// ADR-087). It lives HERE, in the leaf delegate package, rather than in
// pkg/runtime/recovery — which holds the richer run-level taxonomy but
// imports this package, so the dependency cannot run the other way.
// recovery.Classify keeps its own richer codes; this is the narrower,
// shared vocabulary the executor and the DSL agree on.
//
// The categories are deliberately few. Each one must answer "would
// running the SAME prompt against a DIFFERENT model/credential plausibly
// succeed?" — a question that is meaningless for a budget cap or a
// schema-validation failure, which is why neither has a category.
// ---------------------------------------------------------------------------

// FallbackCategory is the coarse reason a delegate call failed, as seen
// by a fallback chain.
type FallbackCategory string

const (
	// FallbackUsageWindow is a subscription/quota WINDOW exhaustion (the
	// forfait 5h or weekly cap). Waiting is the only cure for THIS
	// credential, which makes it the archetypal reason to route
	// elsewhere.
	FallbackUsageWindow FallbackCategory = "usage_window"

	// FallbackAuth is a rejected or expired credential. Distinct from
	// usage_window: no amount of waiting revives it.
	FallbackAuth FallbackCategory = "auth"

	// FallbackUnavailable is a model the credential cannot reach — bad
	// id, unauthorized, or withdrawn. Only claude_code detects it today
	// and it has no typed carrier yet (ADR-087 stage 3), so this
	// category is currently reachable only via ErrModelUnavailable.
	FallbackUnavailable FallbackCategory = "unavailable"

	// FallbackTransientExhausted is a transient condition that survived
	// the in-node retry budget: a throttle, a 5xx, a connectivity blip,
	// an OOM-killed subprocess. It needs no new detection — it is
	// exactly "isDelegateRetryable said yes and the budget still ran
	// out".
	FallbackTransientExhausted FallbackCategory = "transient_exhausted"

	// FallbackUnclassified is everything else, including every error
	// that lost its type on the way here — the sandboxed-claw IPC
	// envelope flattens errors to a string, and kimi/grok have no error
	// channel at all. A chain ADVANCES on this category (preserving the
	// pre-ADR-087 "fall through on any error" behaviour); it can never
	// be named in an `on:` filter, because naming it would be naming
	// "everything the backend failed to describe".
	FallbackUnclassified FallbackCategory = "unclassified"
)

// ErrModelUnavailable marks a model the resolved credential cannot
// reach — a bad or withdrawn id, or one the account is not entitled to.
// Normally non-transient: retrying the same (credential, model) pair cannot
// fix it, which is precisely what makes it a good reason to try the next
// chain element. ResetAt is reserved for a future stage-3 producer that can
// report a temporary provider unavailability window; no shipped backend sets
// it today.
type ErrModelUnavailable struct {
	Provider string // backend that reported it ("claude_code", …)
	Model    string // the model id that was refused
	Detail   string // raw upstream message for diagnostics
	// ResetAt is the provider-reported instant at which a temporary
	// unavailability may clear. Reserved for ADR-087 stage 3; callers must
	// fail open when it is zero.
	ResetAt time.Time
}

func (e *ErrModelUnavailable) Error() string {
	msg := "model unavailable"
	if e.Model != "" {
		msg += " (" + e.Model + ")"
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// ClassifyFallback maps a delegate execution error onto the category a
// fallback chain filters on.
//
// `retryable` tells the classifier whether the in-node retry budget
// considered this error worth re-issuing — the caller already computed
// it, and it is the only way to distinguish "transient, and we burned
// the budget" from "permanent". Pass false when no retry budget was
// spent.
//
// Typed errors win; there is deliberately NO string-needle tier here.
// Needle matching on flattened text belongs at the boundary that
// flattened it (the claw IPC envelope, ADR-087 stage 3 follow-on), not
// in the shared vocabulary — a needle list here would silently reclassify
// an agent quoting an error message in its own prose.
func ClassifyFallback(err error, retryable bool) FallbackCategory {
	if err == nil {
		return FallbackUnclassified
	}
	var unavailable *ErrModelUnavailable
	if errors.As(err, &unavailable) {
		return FallbackUnavailable
	}
	var auth *ErrAuthFailed
	if errors.As(err, &auth) {
		return FallbackAuth
	}
	var rl *ErrRateLimited
	if errors.As(err, &rl) {
		if rl.Kind == RateLimitKindUsageWindow {
			return FallbackUsageWindow
		}
		// Kind "" is contractually transient (legacy, unclassified), and
		// so is RateLimitKindTransient. Either way the budget was spent.
		return FallbackTransientExhausted
	}
	if retryable {
		return FallbackTransientExhausted
	}
	return FallbackUnclassified
}

// IsUsageWindow reports whether err is a provider subscription/quota
// WINDOW exhaustion. Exported because both the in-node retry carve-out
// and the run-level usage-window retry need the same predicate, and
// because a caller must never re-derive it by matching on Kind strings.
func IsUsageWindow(err error) bool {
	var rl *ErrRateLimited
	if !errors.As(err, &rl) {
		return false
	}
	return rl.Kind == RateLimitKindUsageWindow
}
