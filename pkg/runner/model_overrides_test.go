package runner

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
)

// TestWireModelOverridesReachTheEngine walks the last leg of the operator's
// model choice: queue.ModelOverride (what SubmitLaunch published) → the
// persisted row shape → model.ModelOverrides (what the executor enforces).
//
// The regression it guards is a silent one — the runner used to build its
// executor without any overrides at all, so the pod ran the bot's DSL model
// while every UI surface showed the chosen one. Asserting the RESOLVED
// per-node values, not the field copy, is what makes that provable.
func TestWireModelOverridesReachTheEngine(t *testing.T) {
	msg := []queue.ModelOverride{
		{Selector: "assistant", Model: "anthropic/claude-opus-5", Backend: "claude_code", Effort: "ultracode"},
		{Selector: "reviewer_*", Model: "anthropic/claude-fable-5"},
		{Selector: "judge", Provider: "anthropic"},
	}
	o := runview.ModelOverridesFromRun(modelOverridesFromWire(msg))
	if o.Empty() {
		t.Fatal("overrides folded to empty: the pod would run the DSL defaults")
	}

	// Exact node id.
	got := o.ForNode("assistant", ir.NodeAgent)
	if got.Model != "anthropic/claude-opus-5" {
		t.Errorf("assistant model = %q, want anthropic/claude-opus-5", got.Model)
	}
	if got.Backend != "claude_code" {
		t.Errorf("assistant backend = %q, want claude_code", got.Backend)
	}
	if got.Effort != "ultracode" {
		t.Errorf("assistant effort = %q, want ultracode", got.Effort)
	}

	// Glob selector.
	if got := o.ForNode("reviewer_claude", ir.NodeAgent); got.Model != "anthropic/claude-fable-5" {
		t.Errorf("reviewer_claude model = %q, want anthropic/claude-fable-5", got.Model)
	}

	// Kind keyword.
	if got := o.ForNode("verdict", ir.NodeJudge); got.Provider != "anthropic" {
		t.Errorf("judge provider = %q, want anthropic", got.Provider)
	}

	// An unmatched node keeps its DSL values — overrides retarget, never
	// blanket-apply.
	if got := o.ForNode("implement", ir.NodeAgent); got.Model != "" || got.Backend != "" {
		t.Errorf("unmatched node picked up %+v, want zero", got)
	}
}

// A message with no overrides must fold to an empty set, which is what keeps
// BuildExecutor from installing a no-op override layer on every cloud run.
func TestWireModelOverridesEmptyStaysEmpty(t *testing.T) {
	if rows := modelOverridesFromWire(nil); rows != nil {
		t.Errorf("nil wire rows became %+v, want nil", rows)
	}
	if o := runview.ModelOverridesFromRun(modelOverridesFromWire(nil)); !o.Empty() {
		t.Error("nil wire rows produced a non-empty override set")
	}
}
