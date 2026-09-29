package ir

import (
	"slices"
	"testing"
)

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

// AlwaysReaches is AlwaysReachesLLM's walk over any wall: true only when no
// path from the entry to a terminal goes around every wall. A pre-flight
// refuses a run on it, so every shape it cannot walk answers true — and a
// supervisor, not a graph node, is never a wall it invents.
func TestAlwaysReachesWalls(t *testing.T) {
	agent := func(id string) Node { return &AgentNode{BaseNode: BaseNode{ID: id}} }
	graph := func(entry string, edges [][2]string, nodes ...Node) *Workflow {
		w := &Workflow{Entry: entry, Nodes: map[string]Node{"done": &DoneNode{BaseNode: BaseNode{ID: "done"}}}}
		for _, n := range nodes {
			w.Nodes[n.NodeID()] = n
		}
		for _, e := range edges {
			w.Edges = append(w.Edges, &Edge{From: e[0], To: e[1]})
		}
		return w
	}
	wallOn := func(ids ...string) func(Node) bool {
		return func(n Node) bool {
			for _, id := range ids {
				if n.NodeID() == id {
					return true
				}
			}
			return false
		}
	}
	chain := graph("a", [][2]string{{"a", "b"}, {"b", "done"}}, agent("a"), agent("b"))
	branch := graph("pick", [][2]string{{"pick", "a"}, {"pick", "b"}, {"a", "done"}, {"b", "done"}},
		&RouterNode{BaseNode: BaseNode{ID: "pick"}, RouterMode: RouterCondition}, agent("a"), agent("b"))

	for _, tc := range []struct {
		name string
		wf   *Workflow
		wall func(Node) bool
		want bool
	}{
		{"a wall on the only path", chain, wallOn("b"), true},
		{"no wall", chain, wallOn(), false},
		{"a wall one branch avoids", branch, wallOn("a"), false},
		{"a wall on every branch", branch, wallOn("a", "b"), true},
		{"nil workflow", nil, wallOn(), true},
		{"missing entry", graph("ghost", nil, agent("a")), wallOn(), true},
		{"a supervisor is not a wall", func() *Workflow {
			w := graph("a", [][2]string{{"a", "done"}}, agent("a"))
			w.Supervisors = []*Supervisor{{Name: "coach"}}
			return w
		}(), wallOn(), false},
	} {
		if got := tc.wf.AlwaysReaches(tc.wall); got != tc.want {
			t.Errorf("%s: AlwaysReaches = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A fan-out runs every branch: a wall on ONE of them is on every execution,
// so the router is free only when all its branches are. A condition router
// still chooses. The answer does not depend on the order edges were written
// in, and a dangling edge anywhere the entry reaches keeps the walk
// conservative even beside a free path.
func TestAlwaysReachesTakesEveryFanOutBranch(t *testing.T) {
	agent := func(id string) Node { return &AgentNode{BaseNode: BaseNode{ID: id}} }
	wallOnA := func(n Node) bool { return n.NodeID() == "a" }
	shape := func(mode RouterMode, reversed bool) *Workflow {
		edges := []*Edge{
			{From: "split", To: "a"}, {From: "split", To: "b"},
			{From: "a", To: "join"}, {From: "b", To: "join"},
			{From: "join", To: "done"},
		}
		if reversed {
			slices.Reverse(edges)
		}
		return &Workflow{Entry: "split", Edges: edges, Nodes: map[string]Node{
			"split": &RouterNode{BaseNode: BaseNode{ID: "split"}, RouterMode: mode},
			"a":     agent("a"), "b": agent("b"),
			"join": &ToolNode{BaseNode: BaseNode{ID: "join"}, Command: "true"},
			"done": &DoneNode{BaseNode: BaseNode{ID: "done"}},
		}}
	}
	for _, reversed := range []bool{false, true} {
		for _, tc := range []struct {
			mode RouterMode
			want bool
		}{
			{RouterFanOutAll, true},
			{RouterFanOutEach, true},
			{RouterCondition, false},
			{RouterRoundRobin, false},
		} {
			if got := shape(tc.mode, reversed).AlwaysReaches(wallOnA); got != tc.want {
				t.Errorf("%s (edges reversed=%v): AlwaysReaches = %v, want %v", tc.mode, reversed, got, tc.want)
			}
		}
	}
	// The fan-out makes every branch's model call unavoidable for the cap too.
	fanOut := shape(RouterFanOutAll, false)
	fanOut.Nodes["b"] = &ToolNode{BaseNode: BaseNode{ID: "b"}, Command: "true"}
	if !fanOut.AlwaysReachesLLM() {
		t.Error("AlwaysReachesLLM: a fan-out with one model branch spends on every run")
	}

	dangling := &Workflow{Entry: "pick", Edges: []*Edge{{From: "pick", To: "done"}, {From: "pick", To: "ghost"}}, Nodes: map[string]Node{
		"pick": &RouterNode{BaseNode: BaseNode{ID: "pick"}, RouterMode: RouterCondition},
		"done": &DoneNode{BaseNode: BaseNode{ID: "done"}},
	}}
	if !dangling.AlwaysReaches(func(Node) bool { return false }) {
		t.Error("an edge into an undefined node the entry reaches must keep the walk conservative")
	}
}

// CanReach: some execution from the entry reaches the target — conservative
// on a graph the walk refuses.
func TestCanReach(t *testing.T) {
	w := &Workflow{Entry: "a", Edges: []*Edge{{From: "a", To: "b"}, {From: "b", To: "done"}}, Nodes: map[string]Node{
		"a": &ToolNode{BaseNode: BaseNode{ID: "a"}}, "b": &AgentNode{BaseNode: BaseNode{ID: "b"}},
		"c":    &AgentNode{BaseNode: BaseNode{ID: "c"}},
		"done": &DoneNode{BaseNode: BaseNode{ID: "done"}},
	}}
	is := func(id string) func(Node) bool { return func(n Node) bool { return n.NodeID() == id } }
	if !w.CanReach(is("b")) {
		t.Error("b is on the path")
	}
	if w.CanReach(is("c")) {
		t.Error("c is unreachable from the entry")
	}
	if !(&Workflow{Entry: "ghost"}).CanReach(is("b")) {
		t.Error("a graph the walk refuses must answer true")
	}
}
