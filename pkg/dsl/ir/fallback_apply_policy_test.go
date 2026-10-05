package ir

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/llmroute"
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
	}, nil, llmroute.ResolveClasses(nil))
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
	if refusals := ApplyPolicyLadder(wOp, opStages, false, nil, func(n LLMNode) string { return n.GetLLMFields().Model }, nil, llmroute.ResolveClasses(nil)); len(refusals) != 0 {
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
	if refusals := ApplyPolicyLadder(w2, stages, false, nil, func(n LLMNode) string { return n.GetLLMFields().Model }, nil, llmroute.ResolveClasses(nil)); len(refusals) != 0 {
		t.Fatalf("an authored node screened = %v, want silence (the author vetted where it may go)", refusals)
	}
	if got := len(w2.Nodes["implement"].(*AgentNode).Fallbacks); got != 1 {
		t.Fatalf("authored routes disturbed: %d", got)
	}

	// A node whose model maps nothing for the pair skips the stage, named.
	w3 := mk("", "")
	refusals = ApplyPolicyLadder(w3, stages, false, nil, func(n LLMNode) string { return "" }, nil, llmroute.ResolveClasses(nil))
	if got := len(w3.Nodes["implement"].(*AgentNode).Fallbacks); got != 0 {
		t.Fatalf("modelless stages emitted: %d", got)
	}
	if len(refusals) != len(stages) {
		t.Fatalf("refusals = %v, want every modelless stage named", refusals)
	}
}

// The session contract pinned at the screen (ADR-121 §2, the slice-4
// review's F1/F2): a CROSS-backend stage on a session-bearing node is
// refused by name — a silent eviction never happens because the stage
// never lands; a same-backend stage composes, and the launch's backend
// override is what the screen reads (the dispatch reads it first).
func TestApplyPolicyLadder_SessionContract(t *testing.T) {
	// tools: [] makes the claw crossing TOOL-legal so the SESSION refusal
	// is what each case exercises (the tools inversion would otherwise
	// refuse first — also correctly).
	mkSession := func(backend, model string, session SessionMode) *Workflow {
		return &Workflow{Nodes: map[string]Node{"implement": &AgentNode{
			BaseNode:   BaseNode{ID: "implement"},
			LLMFields:  LLMFields{Backend: backend, Model: model},
			Tools:      []string{},
			Session:    session,
			Permission: "ask",
		}}}
	}
	stages := []PolicyLadderStage{{Harness: "claw", Credential: "anthropic_key", On: []string{"usage_window", "auth"}}}

	// Cross-backend on a session node: refused, named.
	w := mkSession("claude_code", "claude-opus-5-5", SessionInherit)
	refusals := ApplyPolicyLadder(w, stages, false, nil, func(n LLMNode) string { return n.GetLLMFields().Model }, nil, llmroute.ResolveClasses(nil))
	if len(refusals) != 1 || !strings.Contains(refusals[0], "session") {
		t.Fatalf("refusals = %v, want the session-continuity refusal naming the crossing", refusals)
	}
	if got := len(w.Nodes["implement"].(*AgentNode).Fallbacks); got != 0 {
		t.Fatalf("stages landed = %d, want none — no silent eviction", got)
	}

	// Same-backend on a session node: composes — the session is carried
	// (the dispatch keeps it; ADR-087 §3's rebuild-and-evict applies only
	// to cross-backend falls, which the screen refuses here).
	w2 := mkSession("claw", "anthropic/claude-opus-5-5", SessionInherit)
	refusals = ApplyPolicyLadder(w2, stages, false, nil, func(n LLMNode) string { return n.GetLLMFields().Model }, nil, llmroute.ResolveClasses(nil))
	if len(refusals) != 0 {
		t.Fatalf("same-backend refused: %v", refusals)
	}
	if got := len(w2.Nodes["implement"].(*AgentNode).Fallbacks); got != 1 {
		t.Fatalf("same-backend stages = %d, want the ladder landed", got)
	}

	// The launch's backend override is what the screen reads (F2): a claw
	// node overridden to claude_code refuses the claw stage on its
	// session — the DSL field alone would have passed it unscreened.
	w3 := mkSession("claw", "anthropic/claude-opus-5-5", SessionInherit)
	refusals = ApplyPolicyLadder(w3, stages, false, nil, func(n LLMNode) string { return n.GetLLMFields().Model }, func(n LLMNode) string {
		return "claude_code"
	}, llmroute.ResolveClasses(nil))
	if len(refusals) != 1 || !strings.Contains(refusals[0], "session") {
		t.Fatalf("override-blind refusals = %v, want the session refusal through the override", refusals)
	}
}

// The crossings' class targets come from the EFFECTIVE table: a level's
// model_classes override changes what a wholesale crossing carries —
// the ladder materializes the override, not just the fold.
func TestApplyPolicyLadder_ClassOverrideBites(t *testing.T) {
	mk := func(model string) *Workflow {
		node := &AgentNode{
			BaseNode:  BaseNode{ID: "implement"},
			LLMFields: LLMFields{Backend: "claude_code", Model: model},
		}
		return &Workflow{Nodes: map[string]Node{"implement": node}}
	}
	stages := []PolicyLadderStage{{Harness: "codex", Credential: "chatgpt_forfait", On: []string{"usage_window"}}}
	w := mk("claude-opus-5-5")
	if r := ApplyPolicyLadder(w, stages, false, nil, func(n LLMNode) string { return n.GetLLMFields().Model }, nil, llmroute.ResolveClasses(nil)); len(r) != 0 {
		t.Fatalf("the shipped-table crossing refused: %v", r)
	}
	if got := w.Nodes["implement"].(*AgentNode).Fallbacks[0].Model; got != "gpt-6-sol" {
		t.Fatalf("shipped-table target = %q, want the top rung gpt-6-sol", got)
	}

	// The override: top/openai = astra — the SAME node's crossing carries
	// it, and the shipped cell would not.
	w2 := mk("claude-opus-5-5")
	overridden := llmroute.ResolveClasses(map[string]map[string]string{"top": {"openai": "gpt-6-astra"}})
	if r := ApplyPolicyLadder(w2, stages, false, nil, func(n LLMNode) string { return n.GetLLMFields().Model }, nil, overridden); len(r) != 0 {
		t.Fatalf("the overridden crossing refused: %v", r)
	}
	if got := w2.Nodes["implement"].(*AgentNode).Fallbacks[0].Model; got != "gpt-6-astra" {
		t.Fatalf("overridden target = %q, want the level's astra", got)
	}
}
