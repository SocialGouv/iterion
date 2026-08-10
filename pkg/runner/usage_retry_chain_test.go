package runner

import (
	"fmt"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// The production error text, verbatim from run 019fea23-9636 (2026-08-10):
// four review runs died on one Anthropic weekly cap inside 90 seconds and NOT
// ONE armed a retry (`retry_state: null` on all four), so four pull requests
// kept a required check nobody would ever answer. The provider window had
// reopened by 19:00 UTC the same day; nothing came back for it.
const prodWeeklyLimitText = "You've hit your weekly limit · resets 7pm (UTC)"

// wrapLikeProduction rebuilds the exact chain a cloud runner receives, layer
// for layer: the delegate's typed error, the executor's backend wrapper
// (executor_build_task.go), and the engine's RuntimeError, whose Message is
// the %v-FLATTENED text and whose Cause is meant to carry the type through
// (engine_exec.go).
func wrapLikeProduction(inner error, code runtime.ErrorCode) error {
	backend := fmt.Errorf("model: node %q: backend %q failed: %w", "reviewer_claude", "claude_code", inner)
	return &runtime.RuntimeError{
		Code:    code,
		NodeID:  "reviewer_claude",
		Cause:   backend,
		Message: fmt.Sprintf("node %q execution failed: %v", "reviewer_claude", backend),
	}
}

// The happy path, pinned so a regression in ANY layer's wrapping shows up
// here: as long as every wrapper uses %w and RuntimeError.Unwrap returns its
// Cause, the typed evidence survives from the delegate to the retry decision.
func TestUsageWindowRetry_TypedErrorSurvivesTheProductionChain(t *testing.T) {
	now := time.Date(2026, 8, 10, 5, 30, 0, 0, time.UTC)
	inner := &delegate.ErrRateLimited{
		Provider: "claude_code",
		Detail:   prodWeeklyLimitText,
		Kind:     delegate.RateLimitKindUsageWindow,
	}
	execErr := wrapLikeProduction(inner, runtime.ErrCodeExecutionFailed)

	at, source, ok := usageWindowRetryAt(execErr, retrypolicy.Normalize(retrypolicy.Policy{}), now)
	if !ok {
		t.Fatalf("no retry armed for a weekly cap — the run parks forever and its PR waits on a check nobody answers")
	}
	if !at.After(now) {
		t.Errorf("retry at %s is not after %s", at, now)
	}
	t.Logf("armed at %s via %s", at, source)
}

// The gap this test exists for. usageWindowRetryAt's own doc promises three
// evidence sources — "the typed error […] the code […] and the flattened
// message is a last resort for a host that has neither — which is not
// hypothetical, since a runner with no dispatcher wired classifies nothing at
// all." The third one was never implemented: usageWindowEvidence returned
// false unless a typed error or a classified code survived.
//
// So on any host where the type does not make it through (a layer that
// formats instead of wrapping, a store round-trip, an out-of-process hop) the
// provider's own words — sitting right there in the message — were ignored,
// and the run parked with nothing coming back for it. That is the shape of the
// four dead reviews above: the text was in run.Error, the retry was null.
func TestUsageWindowRetry_FallsBackToTheProvidersOwnWords(t *testing.T) {
	now := time.Date(2026, 8, 10, 5, 30, 0, 0, time.UTC)
	// A chain that carries the words but NOT the type — what a formatting
	// layer anywhere in the stack leaves behind.
	flattened := &runtime.RuntimeError{
		Code:    runtime.ErrCodeExecutionFailed,
		NodeID:  "reviewer_claude",
		Message: "node \"reviewer_claude\" execution failed: model: node \"reviewer_claude\": backend \"claude_code\" failed: delegate: claude-code failed: rate_limited (claude_code): " + prodWeeklyLimitText,
	}

	at, source, ok := usageWindowRetryAt(flattened, retrypolicy.Normalize(retrypolicy.Policy{}), now)
	if !ok {
		t.Fatalf("the provider said the window is shut and when it reopens, and the run parked with nothing coming back for it")
	}
	// 7pm UTC the same day, plus a minute, plus jitter — must not be the
	// blind one-hour wait when the text names the reset.
	blind := now.Add(usageWindowBlindWait)
	if !at.After(blind) {
		t.Errorf("retry at %s looks like the blind wait (%s) — the parsed reset (7pm) was ignored, source=%s", at, blind, source)
	}
	t.Logf("armed at %s via %s", at, source)
}

// Nothing must widen into "any error is a usage window": an ordinary failure
// that merely MENTIONS a limit, or a plain throttle, still gets today's
// redelivery, not a multi-hour park.
func TestUsageWindowRetry_DoesNotArmOnUnrelatedFailures(t *testing.T) {
	now := time.Date(2026, 8, 10, 5, 30, 0, 0, time.UTC)
	pol := retrypolicy.Normalize(retrypolicy.Policy{})
	for name, msg := range map[string]string{
		"a build failure":           `node "build" execution failed: exit status 1`,
		"an agent discussing quota": `node "review" execution failed: the report explains how to raise your limit and mentions rate limit exceeded handling`,
		"a schema error":            `node "review" execution failed: missing required field "verdict"`,
		"a budget stop":             `budget exceeded: tokens (30/25)`,
		// The anchor's whole job. An agent QUOTING the provider's wording —
		// a review of this very code, a docs bot describing the retry
		// behaviour — must not park its own run for hours. Only a message the
		// delegate itself rendered (marker included) counts as evidence.
		"an agent quoting the notice": `node "review" execution failed: missing required field "verdict" (the model wrote: You've hit your weekly limit · resets 7pm (UTC))`,
	} {
		t.Run(name, func(t *testing.T) {
			err := &runtime.RuntimeError{Code: runtime.ErrCodeExecutionFailed, Message: msg}
			if _, _, ok := usageWindowRetryAt(err, pol, now); ok {
				t.Fatalf("armed a usage-window park for %q — a plain failure now waits hours instead of being redelivered", msg)
			}
		})
	}
}
