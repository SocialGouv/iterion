package ir

import "testing"

func wfWith(nodes ...Node) *Workflow {
	w := &Workflow{Name: "w", Nodes: map[string]Node{}}
	for i, n := range nodes {
		w.Nodes[string(rune('a'+i))] = n
	}
	return w
}

// TestWorkflowUsesLLM covers the predicate kind by kind. The direction that
// matters is asymmetric: a false NEGATIVE hands a model-calling workflow a
// pass around the cap protecting the subscription, so every uncertain shape
// must answer true. A false positive only preserves today's behaviour.
func TestWorkflowUsesLLM(t *testing.T) {
	tests := []struct {
		name string
		wf   *Workflow
		want bool
	}{
		{name: "nil", wf: nil, want: false},
		{name: "empty", wf: wfWith(), want: false},
		{
			name: "an agent node spends",
			wf:   wfWith(&AgentNode{BaseNode: BaseNode{ID: "a"}}),
			want: true,
		},
		{
			name: "a judge node spends",
			wf:   wfWith(&JudgeNode{BaseNode: BaseNode{ID: "a"}}),
			want: true,
		},
		{
			// The Vigie collect shape: tools + compute + terminals only.
			name: "tools and compute alone do not",
			wf: wfWith(
				&ToolNode{BaseNode: BaseNode{ID: "a"}, Command: "true"},
				&ComputeNode{BaseNode: BaseNode{ID: "b"}},
				&DoneNode{BaseNode: BaseNode{ID: "c"}},
				&FailNode{BaseNode: BaseNode{ID: "d"}},
			),
			want: false,
		},
		{
			name: "a deterministic router does not",
			wf:   wfWith(&RouterNode{BaseNode: BaseNode{ID: "a"}, RouterMode: RouterFanOutAll}),
			want: false,
		},
		{
			name: "an llm router does",
			wf:   wfWith(&RouterNode{BaseNode: BaseNode{ID: "a"}, RouterMode: RouterLLM}),
			want: true,
		},
		{
			name: "a human node parking for a human does not",
			wf: wfWith(&HumanNode{BaseNode: BaseNode{ID: "a"},
				InteractionFields: InteractionFields{Interaction: InteractionHuman}}),
			want: false,
		},
		{
			name: "a human node answered by a model does",
			wf: wfWith(&HumanNode{BaseNode: BaseNode{ID: "a"},
				InteractionFields: InteractionFields{Interaction: InteractionLLM}}),
			want: true,
		},
		{
			name: "llm_or_human does — it tries the model first",
			wf: wfWith(&HumanNode{BaseNode: BaseNode{ID: "a"},
				InteractionFields: InteractionFields{Interaction: InteractionLLMOrHuman}}),
			want: true,
		},
		{
			// Rung 4 of a Verified Action hands recovery to an agent. A run
			// that only MIGHT reach it still can.
			name: "a tool node with agent recovery does",
			wf: wfWith(&ToolNode{BaseNode: BaseNode{ID: "a"}, Command: "true",
				Recovery: &RecoverySpec{MaxAgentAttempts: 1}}),
			want: true,
		},
		{
			name: "a tool node whose agent rung is off does not",
			wf: wfWith(&ToolNode{BaseNode: BaseNode{ID: "a"}, Command: "true",
				Recovery: &RecoverySpec{MaxAgentAttempts: 0}}),
			want: false,
		},
		{
			// The child .bot is a separate source this workflow does not
			// carry: unknowable here, so assumed to spend.
			name: "a subbot does — its child is unknowable from here",
			wf:   wfWith(&SubbotNode{BaseNode: BaseNode{ID: "a"}, Source: "child.bot"}),
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.wf.UsesLLM(); got != tc.want {
				t.Errorf("UsesLLM() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWorkflowUsesLLM_SupervisorCountsWithoutAnyLLMNode is the trap: a
// supervisor is an LLM agent watching from the side, so a graph made only of
// tool nodes can still spend. Looking at nodes alone would miss it.
func TestWorkflowUsesLLM_SupervisorCountsWithoutAnyLLMNode(t *testing.T) {
	w := wfWith(&ToolNode{BaseNode: BaseNode{ID: "a"}, Command: "true"})
	if w.UsesLLM() {
		t.Fatal("precondition: a tool-only graph must not count yet")
	}
	w.Supervisors = []*Supervisor{{Name: "watch"}}
	if !w.UsesLLM() {
		t.Error("a supervisor watches with a model — the workflow spends")
	}
}

// TestAlwaysReachesLLM covers the pre-flight predicate. The asymmetry is
// inverted from UsesLLM: a false POSITIVE only preserves today's refusal,
// while a false negative lets a run start that will spend — which the
// mid-run guard still catches, so the cost is a pod, not a bill.
func TestAlwaysReachesLLM(t *testing.T) {
	// entry -> a -> done, with `a` swapped per case.
	build := func(a Node) *Workflow {
		w := &Workflow{
			Name: "w", Entry: "a",
			Nodes: map[string]Node{"a": a, "done": &DoneNode{BaseNode: BaseNode{ID: "done"}}},
			Edges: []*Edge{{From: "a", To: "done"}},
		}
		return w
	}
	if got := build(&ToolNode{BaseNode: BaseNode{ID: "a"}, Command: "true"}).AlwaysReachesLLM(); got {
		t.Error("a tool-only line reaches a terminal without spending")
	}
	if got := build(&AgentNode{BaseNode: BaseNode{ID: "a"}}).AlwaysReachesLLM(); !got {
		t.Error("the only path goes through an agent — every run spends")
	}

	// The shape that matters, and the one the first fix got wrong: ONE
	// workflow, two modes. A router sends collect down a tool-only branch
	// and digest through an agent. Some runs spend, some cannot — so no run
	// may be refused before it has chosen.
	twoMode := &Workflow{
		Name: "two_mode", Entry: "plan",
		Nodes: map[string]Node{
			"plan":  &ToolNode{BaseNode: BaseNode{ID: "plan"}, Command: "true"},
			"fetch": &ToolNode{BaseNode: BaseNode{ID: "fetch"}, Command: "true"},
			"synth": &AgentNode{BaseNode: BaseNode{ID: "synth"}},
			"done":  &DoneNode{BaseNode: BaseNode{ID: "done"}},
			"fail":  &FailNode{BaseNode: BaseNode{ID: "fail"}},
		},
		Edges: []*Edge{
			{From: "plan", To: "fetch", Condition: "collect"},
			{From: "plan", To: "synth", Condition: "digest"},
			{From: "fetch", To: "done"},
			{From: "synth", To: "done"},
			{From: "plan", To: "fail"},
		},
	}
	if twoMode.AlwaysReachesLLM() {
		t.Error("a two-mode workflow has a model-free path; it must not be refused in advance")
	}
	// It still CONTAINS an LLM node — the two predicates must disagree here,
	// which is the entire reason both exist.
	if !twoMode.UsesLLM() {
		t.Error("UsesLLM must still see the agent node")
	}

	// Conservative shapes: refuse to conclude "free path" from a graph this
	// cannot walk.
	for name, w := range map[string]*Workflow{
		"nil":           nil,
		"no nodes":      {Name: "w", Entry: "a"},
		"missing entry": {Name: "w", Entry: "ghost", Nodes: map[string]Node{"a": &ToolNode{BaseNode: BaseNode{ID: "a"}}}},
		"dangling edge": {Name: "w", Entry: "a", Nodes: map[string]Node{"a": &ToolNode{BaseNode: BaseNode{ID: "a"}}}, Edges: []*Edge{{From: "a", To: "ghost"}}},
		"dead end":      {Name: "w", Entry: "a", Nodes: map[string]Node{"a": &ToolNode{BaseNode: BaseNode{ID: "a"}}}},
	} {
		if !w.AlwaysReachesLLM() {
			t.Errorf("%s: must stay conservative and answer true", name)
		}
	}

	// A supervisor spends whatever path the graph takes.
	sup := build(&ToolNode{BaseNode: BaseNode{ID: "a"}, Command: "true"})
	sup.Supervisors = []*Supervisor{{Name: "watch"}}
	if !sup.AlwaysReachesLLM() {
		t.Error("a supervisor is armed on every path")
	}

	// A cycle must not hang the walk.
	loop := &Workflow{
		Name: "loop", Entry: "a",
		Nodes: map[string]Node{
			"a": &ToolNode{BaseNode: BaseNode{ID: "a"}, Command: "true"},
			"b": &ToolNode{BaseNode: BaseNode{ID: "b"}, Command: "true"},
		},
		Edges: []*Edge{{From: "a", To: "b"}, {From: "b", To: "a"}},
	}
	if !loop.AlwaysReachesLLM() {
		t.Error("a cycle reaching no terminal proves no free path")
	}
}
