package ir

import (
	"strings"
	"testing"
)

// The policy ladder's screen: the same eligibility ApplyRunFallback
// enforces (agent nodes only, own routes excluded), the model computed per
// node (a pair with no mapping for THIS node is skipped, named — never
// emitted modelless), and the stage's On carried verbatim: the resolved
// policy's trigger set, auth included — the chain's default set would stop
// the selection's own scenario.
func TestApplyPolicyLadder(t *testing.T) {
	mk := func(model string, permission string) *Workflow {
		node := &AgentNode{
			BaseNode:   BaseNode{ID: "implement"},
			LLMFields:  LLMFields{Backend: "claude_code", Model: model},
			Permission: permission,
		}
		return &Workflow{Nodes: map[string]Node{"implement": node}}
	}
	stages := []PolicyLadderStage{
		{Harness: "claude_code", Credential: "anthropic_key", On: []string{"usage_window", "auth"}},
		{Harness: "claw", Credential: "anthropic_key", On: []string{"usage_window", "auth"}},
	}

	w := mk("claude-opus-5-5", "")
	refusals := ApplyPolicyLadder(w, stages, false, nil, func(n LLMNode) string {
		return n.GetLLMFields().Model
	})
	node := w.Nodes["implement"].(*AgentNode)
	if len(node.Fallbacks) != 1 {
		t.Fatalf("stages = %d (%+v), refusals = %v — want 1 (the claw crossing is refused: the node has no permission gate or claw-eligible toolset), refusals naming it", len(node.Fallbacks), node.Fallbacks, refusals)
	}
	st := node.Fallbacks[0]
	if st.Backend != "claude_code" || st.Provider != "anthropic" || !st.Policy {
		t.Fatalf("stage = %+v, want claude_code hinted anthropic, Policy-flagged", st)
	}
	if len(st.On) != 2 || st.On[1] != "auth" {
		t.Fatalf("On = %v, want the policy's trigger set (auth carried — the default set would refuse it)", st.On)
	}
	if !st.RunStageSet || st.RunStage != 0 {
		t.Fatalf("stage position not stamped: %+v", st)
	}
	if len(refusals) != 1 || !strings.Contains(refusals[0], "claw") {
		t.Fatalf("refusals = %v, want the claw crossing named", refusals)
	}

	// The COMPOSITION (the slice-3 review's C2): a node the OPERATOR
	// screen armed moments earlier still takes the ladder — the
	// eligibility is authored routes only. Operator stages carry
	// RunStageSet; the ladder appends after them with a continuing index.
	wOp := mk("claude-opus-5-5", "")
	wn := wOp.Nodes["implement"].(*AgentNode)
	wn.Fallbacks = append(wn.Fallbacks, Fallback{Name: RunFallbackName, Backend: "claude_code", Model: "claude-opus-5-5", RunStage: 0, RunStageSet: true})
	opStages := stages[:1] // the claude_code crossing only: the claw stage needs a permission gate this node does not carry
	if refusals := ApplyPolicyLadder(wOp, opStages, false, nil, func(n LLMNode) string { return n.GetLLMFields().Model }); len(refusals) != 0 {
		t.Fatalf("operator-armed node refused the ladder: %v", refusals)
	}
	if got := len(wn.Fallbacks); got != 2 {
		t.Fatalf("operator-armed node stages = %d, want operator + ladder composed", got)
	}
	if last := wn.Fallbacks[1]; !last.Policy || last.RunStage != 1 {
		t.Fatalf("ladder stage = %+v, want Policy with the RunStage index continuing (1)", last)
	}

	// A node with routes of its own takes nothing.
	w2 := mk("claude-opus-5-5", "")
	w2.Nodes["implement"] = &AgentNode{
		BaseNode:  BaseNode{ID: "implement"},
		LLMFields: LLMFields{Backend: "claude_code", Model: "claude-opus-5-5"},
		Fallbacks: []Fallback{{Name: "authored", Backend: "claude_code", Model: "claude-opus-5-5"}},
	}
	if refusals := ApplyPolicyLadder(w2, stages, false, nil, func(n LLMNode) string { return n.GetLLMFields().Model }); len(refusals) != 0 {
		t.Fatalf("an authored node screened = %v, want silence (the author vetted where it may go)", refusals)
	}
	if got := len(w2.Nodes["implement"].(*AgentNode).Fallbacks); got != 1 {
		t.Fatalf("authored routes disturbed: %d", got)
	}

	// A node whose model maps nothing for the pair skips the stage, named.
	w3 := mk("", "")
	refusals = ApplyPolicyLadder(w3, stages, false, nil, func(n LLMNode) string { return "" })
	if got := len(w3.Nodes["implement"].(*AgentNode).Fallbacks); got != 0 {
		t.Fatalf("modelless stages emitted: %d", got)
	}
	if len(refusals) != len(stages) {
		t.Fatalf("refusals = %v, want every modelless stage named", refusals)
	}
}
