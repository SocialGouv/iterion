package recovery

import (
	"context"
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestCapabilityUnsupportedDoesNotRetry(t *testing.T) {
	err := fmt.Errorf("dispatch: %w", &delegate.ErrCapabilityUnsupported{NodeID: "asker", Backend: "codex", Capability: "interaction: async"})
	if got := Classify(err); got != runtime.ErrCodeCapabilityUnsupported {
		t.Fatalf("classification = %s", got)
	}
	for _, attempts := range []int{0, 1, 10} {
		action, code := Dispatch(DefaultRecipes())(context.Background(), err, func(runtime.ErrorCode) int { return attempts })
		if action.Kind != runtime.RecoveryFailTerminal || code != runtime.ErrCodeCapabilityUnsupported {
			t.Fatalf("attempt %d: action=%+v code=%s", attempts, action, code)
		}
	}
	if !retrypolicy.IsDeterministic(store.FailureCapabilityUnsupported) || retrypolicy.AutoResumable(store.FailureCapabilityUnsupported) {
		t.Fatal("a missing capability requires a configuration change before resuming")
	}
}

// TestBackendCLIAbsentIsTheSameClassOnTheHostPlane: the dispatch-plane refusal
// of a backend no published image ships (#1643) classifies to the same
// deterministic terminal code as the declaration-plane refusal — retrying
// cannot install a CLI.
func TestBackendCLIAbsentIsTheSameClassOnTheHostPlane(t *testing.T) {
	err := fmt.Errorf("dispatch: %w", &delegate.ErrBackendCLIAbsent{NodeID: "implement", Backend: "kimi", Binary: "kimi"})
	if got := Classify(err); got != runtime.ErrCodeCapabilityUnsupported {
		t.Fatalf("classification = %s", got)
	}
	action, code := Dispatch(DefaultRecipes())(context.Background(), err, func(runtime.ErrorCode) int { return 0 })
	if action.Kind != runtime.RecoveryFailTerminal || code != runtime.ErrCodeCapabilityUnsupported {
		t.Fatalf("action=%+v code=%s", action, code)
	}
}
