package cli

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// An undecided effect anywhere in the chain is the code the auto-resume gate
// reads, whatever a wrapper said.
func TestRuntimeCodeReadsAnUndecidedEffectThroughItsWrapper(t *testing.T) {
	err := &runtime.RuntimeError{Code: runtime.ErrCodeExecutionFailed, Cause: &sandbox.ExecStatusUnknownError{Swept: false}}
	if got := runtimeCode(err); got != runtime.ErrCodeAmbiguousEffect {
		t.Fatalf("runtimeCode = %q, want %q", got, runtime.ErrCodeAmbiguousEffect)
	}
}
