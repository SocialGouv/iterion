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
