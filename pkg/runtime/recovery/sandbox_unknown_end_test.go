package recovery

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// A sandboxed tool command whose end is unknown and whose sweep could not
// finish may still be running: recovery classifies it as an ambiguous effect,
// whose recipe never retries — a retry would start a second copy beside the
// first. One whose sweep finished is an ordinary failure.
func TestClassifyASandboxedCommandsUnknownEnd(t *testing.T) {
	nodeErr := func(swept bool) error {
		return fmt.Errorf("model: tool node %q: shell command failed: %w", "gate",
			errors.Join(errors.New("exit status 1"), &sandbox.ExecStatusUnknownError{Swept: swept, Detail: "the sweep could not finish"}))
	}
	code := Classify(nodeErr(false))
	if code != runtime.ErrCodeAmbiguousEffect {
		t.Fatalf("an unknown end with a failed sweep classifies as %q, want %q", code, runtime.ErrCodeAmbiguousEffect)
	}
	if act := DefaultRecipes()[code].Apply(context.Background(), &runtime.RuntimeError{Code: code}, 0); act.Kind != ActionFailTerminal {
		t.Fatalf("the ambiguous effect's recipe acts %v, want a terminal failure", act.Kind)
	}
	if code := Classify(nodeErr(true)); code == runtime.ErrCodeAmbiguousEffect {
		t.Fatal("an unknown end whose sweep finished classifies as an ambiguous effect")
	}
}
