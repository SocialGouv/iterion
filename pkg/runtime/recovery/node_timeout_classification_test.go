package recovery

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// What each backend hands up when the node's own `timeout:` cuts it. The
// three accounts differ in every way a text matcher cares about, which is
// the point: the classification must not depend on which one came back.
var cutAccounts = map[string]error{
	// A sandboxed claw crossing reports what the far side saw. No context
	// error in the chain, no deadline in the words.
	"sandboxed claw": errors.New("claw backend: runner exited with error: signal: killed (stderr: )"),
	// An in-process claw call, or the in-node retry loop whose backoff the
	// bound cut: the bare context error, whose text IS a network-transient
	// needle.
	"in-process claw": fmt.Errorf("model: node %q: backend %q failed: %w", "slow", "claw", context.DeadlineExceeded),
	// The claude_code CLI subprocess, killed with its node.
	"claude_code": errors.New("claude_code: cli exited: signal: killed"),
}

// A node ended by the bound its own `timeout:` declared classifies as
// TIMEOUT whatever its backend's account of the cut reads like — read by
// type, never by words.
//
// Mutation that reddens this (reader): delete the `*model.ErrNodeDeadline`
// arm from Classify — the in-process account's text matches the
// "context deadline exceeded" needle and returns NETWORK_TRANSIENT (whose
// recipe re-runs the node six times), the two others fall to the catch-all
// EXECUTION_FAILED.
func TestANodeCutByItsOwnTimeoutClassifiesByTypeNotByWords(t *testing.T) {
	for voice, observed := range cutAccounts {
		t.Run(voice, func(t *testing.T) {
			cut := &model.ErrNodeDeadline{NodeID: "slow", Timeout: 20 * time.Millisecond, Observed: observed}
			if got := Classify(cut); got != runtime.ErrCodeTimeout {
				t.Errorf("Classify = %s, want %s — the bound the node declared expiring is that bound expiring, whatever the backend said", got, runtime.ErrCodeTimeout)
			}
		})
	}
}

// The needle stays for what it was written for: a request-level timeout
// INSIDE the node is the upstream client's own bound, says nothing about the
// node's, and remains the transient fault the longer backoff absorbs.
// Narrowing the needle instead of reading the type is the shape that was
// measured and reverted (#2456, patch 2).
func TestAnUpstreamRequestTimeoutStaysANetworkTransient(t *testing.T) {
	err := errors.New(`Post "https://api.anthropic.com/v1/messages": context deadline exceeded`)
	if got := Classify(err); got != runtime.ErrCodeNetworkTransient {
		t.Errorf("Classify = %s, want %s — an upstream request timeout is not the node's bound", got, runtime.ErrCodeNetworkTransient)
	}
}

// A cut that wraps a typed refusal is still that refusal: the node's bound
// may be what ended the attempt, but a usage window is what an operator (and
// the run-level usage retry) can act on. The typed branches sit ABOVE the
// cut's arm for exactly this.
func TestACutWrappingAUsageWindowStaysTheUsageWindow(t *testing.T) {
	window := &delegate.ErrRateLimited{Kind: delegate.RateLimitKindUsageWindow}
	cut := &model.ErrNodeDeadline{NodeID: "slow", Timeout: time.Minute, Observed: window}
	if got := Classify(cut); got != runtime.ErrCodeUsageLimitBlocked {
		t.Errorf("Classify = %s, want %s — the window is what the operator can act on", got, runtime.ErrCodeUsageLimitBlocked)
	}
}

// The registry answers for the code, and its answer is "not again": a retry
// would run the node under a FRESH bound, turning the one duration the
// author wrote into N of them.
func TestTheNodeTimeoutRecipeNeverRerunsTheNodeItCut(t *testing.T) {
	recipes := DefaultRecipes()
	recipe, ok := recipes[runtime.ErrCodeTimeout]
	if !ok {
		t.Fatalf("no recipe registered for %s — Dispatch would fail terminal with a reason that names the gap instead of the bound", runtime.ErrCodeTimeout)
	}
	for attempts := 0; attempts < 3; attempts++ {
		act := recipe.Apply(context.Background(), &runtime.RuntimeError{Code: runtime.ErrCodeTimeout}, attempts)
		if act.Kind != ActionFailTerminal {
			t.Errorf("attempt %d: %v, want FailTerminal — the bound is the author's", attempts, act.Kind)
		}
	}
}

// End to end through the dispatcher production wires (pkg/runner/loop.go):
// one classification, one decision, no re-run.
func TestDispatchEndsANodeCutByItsOwnTimeoutOnce(t *testing.T) {
	dispatch := Dispatch(DefaultRecipes())
	cut := &model.ErrNodeDeadline{NodeID: "slow", Timeout: 20 * time.Millisecond, Observed: cutAccounts["in-process claw"]}
	act, code := dispatch(context.Background(), cut, func(runtime.ErrorCode) int { return 0 })
	if code != runtime.ErrCodeTimeout {
		t.Errorf("code = %s, want %s", code, runtime.ErrCodeTimeout)
	}
	if act.Kind != runtime.RecoveryFailTerminal {
		t.Errorf("action = %v, want FailTerminal — a cut node is not re-run", act.Kind)
	}
}
