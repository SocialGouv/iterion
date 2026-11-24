package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/permissions"
	"github.com/SocialGouv/iterion/pkg/scripthost"
)

// CM-8: a bash call made from inside a codemode program re-caps its output to
// the script's result budget armed on the call's context, instead of the
// tool's historical 10 KiB — so a script whose bound is 64 KiB can read a
// build log up to its bound, and the cut still says so. The direct path
// (no budget armed) keeps 10 KiB byte for byte.

func TestBashFollowsTheScriptBudgetArmedOnItsContext(t *testing.T) {
	const budget = 100
	ctx := scripthost.WithScriptResultBudget(context.Background(), budget)
	out, err := ExecuteBashWithEnv(ctx, map[string]any{"command": "head -c 1000 /dev/zero | tr '\\0' 'x'"},
		permissions.ModeAllow, "", nil)
	if err != nil {
		t.Fatalf("command must succeed: %v", err)
	}
	want := strings.Repeat("x", budget) + "\n... [output truncated]"
	if out != want {
		t.Fatalf("a %d-byte budget produced %d bytes of output, want the budget-sized prefix and the cut named", budget, len(out))
	}
}

// A command that fills the budget exactly is complete output, not a cut.
func TestBashAtExactlyTheBudgetIsNotTruncated(t *testing.T) {
	const budget = 512
	ctx := scripthost.WithScriptResultBudget(context.Background(), budget)
	out, err := ExecuteBashWithEnv(ctx, map[string]any{"command": "head -c 512 /dev/zero | tr '\\0' 'x'"},
		permissions.ModeAllow, "", nil)
	if err != nil {
		t.Fatalf("command must succeed: %v", err)
	}
	if out != strings.Repeat("x", budget) {
		t.Fatalf("output of %d bytes is not the exact %d-byte budget fill: %t", len(out), budget, strings.HasSuffix(out, "[output truncated]"))
	}
}

// Without a budget on the context — the direct path — the cap is today's
// 10 KiB, and a run over it is cut with the same metadata as before CM-8.
func TestBashWithoutABudgetKeepsItsOwnCap(t *testing.T) {
	out, err := ExecuteBashWithEnv(context.Background(), map[string]any{"command": "head -c 20000 /dev/zero | tr '\\0' 'x'"},
		permissions.ModeAllow, "", nil)
	if err != nil {
		t.Fatalf("command must succeed: %v", err)
	}
	want := strings.Repeat("x", maxOutputSize) + "\n... [output truncated]"
	if out != want {
		t.Fatalf("a direct call produced %d bytes, want the historical %d-byte prefix and the cut named", len(out), maxOutputSize)
	}
}

// A budget above the scripthost platform ceiling is held to it for the
// ALLOCATION: the script host clips a nested result frame at the same bound,
// so a larger buffer is never observable, and a hard ceiling is what keeps
// the fixed-buffer property under a hostile or mistaken context value.
func TestBashCapIsHeldToThePlatformCeiling(t *testing.T) {
	ctx := scripthost.WithScriptResultBudget(context.Background(), 8<<20)
	if got, want := bashOutputCap(ctx), scripthost.Ceiling().ToolResultBytes; got != int(want) {
		t.Fatalf("bashOutputCap(8 MiB budget) = %d, want the %d-byte platform ceiling", got, want)
	}
	if got := bashOutputCap(context.Background()); got != maxOutputSize {
		t.Fatalf("bashOutputCap(no budget) = %d, want the historical %d", got, maxOutputSize)
	}
}
