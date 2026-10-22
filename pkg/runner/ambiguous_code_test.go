package runner

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// An undecided effect anywhere in the chain is the code the runner acts on,
// whatever a wrapper said: redelivering it may duplicate the effect.
func TestRuntimeCodeOfReadsAnUndecidedEffectThroughItsWrapper(t *testing.T) {
	err := &runtime.RuntimeError{Code: runtime.ErrCodeExecutionFailed, Cause: &sandbox.ExecStatusUnknownError{Swept: false}}
	if got := runtimeCodeOf(err); got != runtime.ErrCodeAmbiguousEffect {
		t.Fatalf("runtimeCodeOf = %q, want %q", got, runtime.ErrCodeAmbiguousEffect)
	}
}
