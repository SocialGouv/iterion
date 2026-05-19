package runtime

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// ---------------------------------------------------------------------------
// isMutatingNode — workspace mutation classifier
// ---------------------------------------------------------------------------

func TestIsMutatingNode_ToolNodeAlwaysMutating(t *testing.T) {
	if !isMutatingNode(&ir.ToolNode{BaseNode: ir.BaseNode{ID: "tool"}}) {
		t.Error("tool node must be classified mutating")
	}
}

func TestIsMutatingNode_AgentReadonlyOverride(t *testing.T) {
	// Even if the agent declares tools, Readonly=true wins.
	n := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "a"},
		LLMFields: ir.LLMFields{Readonly: true},
		Tools:     []string{"bash", "edit_file"},
	}
	if isMutatingNode(n) {
		t.Error("readonly agent must not be classified mutating regardless of tools")
	}
}

func TestIsMutatingNode_AgentWithOnlyReadOnlyTools(t *testing.T) {
	n := &ir.AgentNode{
		BaseNode: ir.BaseNode{ID: "a"},
		Tools:    []string{"git_diff", "git_status", "read_file", "list_files"},
	}
	if isMutatingNode(n) {
		t.Errorf("agent with only readOnlyTools should not be mutating; got mutating=true")
	}
}

func TestIsMutatingNode_AgentWithOneMutatingTool(t *testing.T) {
	n := &ir.AgentNode{
		BaseNode: ir.BaseNode{ID: "a"},
		Tools:    []string{"git_diff", "edit_file"}, // edit_file is not in readOnlyTools
	}
	if !isMutatingNode(n) {
		t.Error("agent with at least one non-readonly tool must be mutating")
	}
}

func TestIsMutatingNode_AgentWithNoTools(t *testing.T) {
	// An agent without any tools cannot mutate via the executor.
	n := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}}
	if isMutatingNode(n) {
		t.Error("agent with no tools should not be mutating")
	}
}

func TestIsMutatingNode_JudgeMatchesAgentSemantics(t *testing.T) {
	// Judge with all-readonly tools is safe.
	readonly := &ir.JudgeNode{
		BaseNode: ir.BaseNode{ID: "j"},
		Tools:    []string{"read_file"},
	}
	if isMutatingNode(readonly) {
		t.Error("judge with readonly tools should not be mutating")
	}
	// Judge with a mutating tool is mutating.
	mutating := &ir.JudgeNode{
		BaseNode: ir.BaseNode{ID: "j"},
		Tools:    []string{"bash"},
	}
	if !isMutatingNode(mutating) {
		t.Error("judge with non-readonly tool must be mutating")
	}
	// Readonly judge wins.
	override := &ir.JudgeNode{
		BaseNode:  ir.BaseNode{ID: "j"},
		LLMFields: ir.LLMFields{Readonly: true},
		Tools:     []string{"bash"},
	}
	if isMutatingNode(override) {
		t.Error("readonly judge must not be mutating regardless of tools")
	}
}

func TestIsMutatingNode_NonExecutableNodesNotMutating(t *testing.T) {
	cases := []ir.Node{
		&ir.RouterNode{BaseNode: ir.BaseNode{ID: "r"}, RouterMode: ir.RouterFanOutAll},
		&ir.HumanNode{BaseNode: ir.BaseNode{ID: "h"}},
		&ir.ComputeNode{BaseNode: ir.BaseNode{ID: "c"}},
		&ir.DoneNode{BaseNode: ir.BaseNode{ID: "d"}},
		&ir.FailNode{BaseNode: ir.BaseNode{ID: "f"}},
	}
	for _, n := range cases {
		if isMutatingNode(n) {
			t.Errorf("%T should not be mutating", n)
		}
	}
}

// ---------------------------------------------------------------------------
// findConvergencePoint — global join discovery
// ---------------------------------------------------------------------------

func TestFindConvergencePoint_TwoBranchesMeetAtNode(t *testing.T) {
	wf := &ir.Workflow{
		Name: "t",
		Nodes: map[string]ir.Node{
			"router": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"a":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"join":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "join"}, AwaitMode: ir.AwaitWaitAll},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "router", To: "a"},
			{From: "router", To: "b"},
			{From: "a", To: "join"},
			{From: "b", To: "join"},
			{From: "join", To: "done"},
		},
	}
	e := &Engine{workflow: wf}
	got := e.findConvergencePoint("router", []*ir.Edge{
		{From: "router", To: "a"},
		{From: "router", To: "b"},
	})
	if got != "join" {
		t.Errorf("expected convergence=join, got %q", got)
	}
}

func TestFindConvergencePoint_BranchesGoDirectlyToDone(t *testing.T) {
	wf := &ir.Workflow{
		Name: "t",
		Nodes: map[string]ir.Node{
			"router": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"a":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "router", To: "a"},
			{From: "router", To: "b"},
			{From: "a", To: "done"},
			{From: "b", To: "done"},
		},
	}
	e := &Engine{workflow: wf}
	got := e.findConvergencePoint("router", []*ir.Edge{
		{From: "router", To: "a"},
		{From: "router", To: "b"},
	})
	if got != "done" {
		t.Errorf("expected convergence=done (terminal convergence), got %q", got)
	}
}

func TestFindConvergencePoint_NoConvergenceReturnsEmpty(t *testing.T) {
	// Each branch terminates in its own done node — no shared convergence.
	wf := &ir.Workflow{
		Name: "t",
		Nodes: map[string]ir.Node{
			"router":  &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"a":       &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b":       &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"done_a":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done_a"}},
			"done_b":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done_b"}},
		},
		Edges: []*ir.Edge{
			{From: "router", To: "a"},
			{From: "router", To: "b"},
			{From: "a", To: "done_a"},
			{From: "b", To: "done_b"},
		},
	}
	e := &Engine{workflow: wf}
	got := e.findConvergencePoint("router", []*ir.Edge{
		{From: "router", To: "a"},
		{From: "router", To: "b"},
	})
	if got != "" {
		t.Errorf("expected no convergence, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// branchContainsMutation — uses findConvergencePoint and isMutatingNode
// ---------------------------------------------------------------------------

func TestBranchContainsMutation_DetectsMutationBetweenIntermediateJoinAndGlobalConvergence(t *testing.T) {
	// Regression coverage for the comment in fan_out.go around line 782:
	// the BFS used to stop at the first AwaitMode != AwaitNone node and
	// miss mutating nodes between that intermediate join and the global
	// convergence point.
	wf := &ir.Workflow{
		Name: "t",
		Nodes: map[string]ir.Node{
			"router":      &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"a":           &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"join_a":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "join_a"}, AwaitMode: ir.AwaitWaitAll},
			"mut_a":       &ir.ToolNode{BaseNode: ir.BaseNode{ID: "mut_a"}, Command: "git commit"},
			"b":           &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"global_join": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "global_join"}, AwaitMode: ir.AwaitWaitAll},
			"done":        &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "router", To: "a"},
			{From: "router", To: "b"},
			{From: "a", To: "join_a"},
			{From: "join_a", To: "mut_a"},
			{From: "mut_a", To: "global_join"},
			{From: "b", To: "global_join"},
			{From: "global_join", To: "done"},
		},
	}
	e := &Engine{workflow: wf}
	// Branch A reaches mut_a (mutating) before global_join.
	if !e.branchContainsMutation("a", "global_join") {
		t.Error("BFS must catch mutation after an intermediate join, before the global convergence")
	}
	// Branch B has no mutation up to global_join.
	if e.branchContainsMutation("b", "global_join") {
		t.Error("branch B has no mutation; got mutation=true")
	}
}

func TestBranchContainsMutation_StopsAtTerminalNode(t *testing.T) {
	// A branch that ends in done before reaching the global convergence
	// shouldn't crash or loop — terminal nodes stop the walk.
	wf := &ir.Workflow{
		Name: "t",
		Nodes: map[string]ir.Node{
			"a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{{From: "a", To: "done"}},
	}
	e := &Engine{workflow: wf}
	if e.branchContainsMutation("a", "") {
		t.Error("branch ending in done before global convergence should not report mutation")
	}
}

// ---------------------------------------------------------------------------
// validateWorkspaceSafety — top-level safety gate
// ---------------------------------------------------------------------------

func TestValidateWorkspaceSafety_RejectsTwoMutatingBranches(t *testing.T) {
	wf := &ir.Workflow{
		Name: "t",
		Nodes: map[string]ir.Node{
			"router": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"tool_a": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "tool_a"}, Command: "git commit"},
			"tool_b": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "tool_b"}, Command: "git push"},
			"join":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "join"}, AwaitMode: ir.AwaitWaitAll},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "router", To: "tool_a"},
			{From: "router", To: "tool_b"},
			{From: "tool_a", To: "join"},
			{From: "tool_b", To: "join"},
			{From: "join", To: "done"},
		},
	}
	e := &Engine{workflow: wf}
	err := e.validateWorkspaceSafety("router", []*ir.Edge{
		{From: "router", To: "tool_a"},
		{From: "router", To: "tool_b"},
	})
	if err == nil {
		t.Fatal("expected workspace safety violation, got nil")
	}
	var rtErr *RuntimeError
	ok := errorAs(err, &rtErr)
	if !ok || rtErr.Code != ErrCodeWorkspaceSafety {
		t.Errorf("expected RuntimeError ErrCodeWorkspaceSafety, got %v", err)
	}
}

func TestValidateWorkspaceSafety_AllowsOneMutatingPlusReadOnly(t *testing.T) {
	wf := &ir.Workflow{
		Name: "t",
		Nodes: map[string]ir.Node{
			"router":    &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"mutating":  &ir.ToolNode{BaseNode: ir.BaseNode{ID: "mutating"}, Command: "git commit"},
			"read_only": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "read_only"}, LLMFields: ir.LLMFields{Readonly: true}, Tools: []string{"bash"}},
			"join":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "join"}, AwaitMode: ir.AwaitWaitAll},
			"done":      &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "router", To: "mutating"},
			{From: "router", To: "read_only"},
			{From: "mutating", To: "join"},
			{From: "read_only", To: "join"},
			{From: "join", To: "done"},
		},
	}
	e := &Engine{workflow: wf}
	if err := e.validateWorkspaceSafety("router", []*ir.Edge{
		{From: "router", To: "mutating"},
		{From: "router", To: "read_only"},
	}); err != nil {
		t.Errorf("one mutating + one readonly should be allowed, got %v", err)
	}
}

// errorAs is a tiny local errors.As shim (avoids extra import).
func errorAs(err error, target **RuntimeError) bool {
	if err == nil {
		return false
	}
	if rt, ok := err.(*RuntimeError); ok {
		*target = rt
		return true
	}
	return false
}
