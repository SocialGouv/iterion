package runner

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
)

// modelOverridesFromMsg is the runner-side fold of the wire pins into the
// executor's override set — the piece whose ABSENCE made cloud launches'
// model_overrides display-only. Falsified both ways: entries resolve for
// a matching node; an empty wire yields the zero set (a run with no pins
// behaves exactly as before).
func TestModelOverridesFromMsg(t *testing.T) {
	o := modelOverridesFromMsg([]queue.ModelOverride{
		{Selector: "agent", Backend: "claude_code", Model: "claude-fable-5"},
		{Selector: "reviewer_*", Model: "claude-opus-5"},
	})
	if o.Empty() {
		t.Fatal("two wire entries folded into an empty override set")
	}
	agent := o.ForNode("campaign", ir.NodeAgent)
	if agent.Model != "claude-fable-5" || agent.Backend != "claude_code" {
		t.Fatalf("agent-kind node resolved %+v, want the fable pin", agent)
	}
	rev := o.ForNode("reviewer_claude", ir.NodeJudge)
	if rev.Model != "claude-opus-5" {
		t.Fatalf("glob-matched node resolved %+v, want the opus pin", rev)
	}

	if !modelOverridesFromMsg(nil).Empty() {
		t.Fatal("an empty wire must fold into the zero override set")
	}
}

// runFallbackFromMsg is the runner-side fold of the wire route into the
// IR form ir.ApplyRunFallback screens — the run-level fallback twin of
// modelOverridesFromMsg. The Name is stamped here so every consumer
// reports the route under the recognisable launch-route label.
func TestRunFallbackFromMsg(t *testing.T) {
	chain := runFallbackFromMsg(queue.RunFallback{
		{Backend: "codex", Model: "gpt-5.5", Provider: "openai"},
		{Backend: "claw", Model: "anthropic/claude-opus-5", Provider: "anthropic"},
	})
	if len(chain) != 2 {
		t.Fatalf("folded chain = %+v, want two stages", chain)
	}
	if f := chain[0]; f.Name != ir.RunFallbackName || f.Backend != "codex" || f.Model != "gpt-5.5" || f.Provider != "openai" {
		t.Fatalf("first folded stage = %+v, want the first wire entry under the run-fallback label", f)
	}
	if f := chain[1]; f.Name != ir.RunFallbackName || f.Backend != "claw" || f.Model != "anthropic/claude-opus-5" || f.Provider != "anthropic" {
		t.Fatalf("second folded stage = %+v, want order preserved", f)
	}
	if z := runFallbackFromMsg(nil); z != nil {
		t.Fatalf("nil wire chain folded to %+v, want nil", z)
	}
}
