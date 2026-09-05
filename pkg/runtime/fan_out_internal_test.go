package runtime

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// ---------------------------------------------------------------------------
// isMutatingNode — workspace mutation classifier
// ---------------------------------------------------------------------------

func TestIsMutatingNode_ToolNodeMutatingByDefault(t *testing.T) {
	if !isMutatingNode(&ir.ToolNode{BaseNode: ir.BaseNode{ID: "tool"}}) {
		t.Error("tool node must be classified mutating by default")
	}
}

func TestIsMutatingNode_ToolParallelSafeScopedToFanOutEach(t *testing.T) {
	// `parallel_safe:` relaxes a tool to non-mutating ONLY on a fan_out_each
	// template (item-keyed disjoint replays). The general classifier — used by
	// the static fan_out_all / llm-router guard, where branches are distinct
	// nodes with no item-key guarantee — keeps it conservatively mutating.
	n := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "tool"}, ParallelSafe: true}
	if !isMutatingNode(n) {
		t.Error("parallel_safe tool must stay mutating in the general (non-fan_out_each) classifier")
	}
	if isMutatingNodeCtx(n, "", nil, true) {
		t.Error("parallel_safe tool must be exempt on a fan_out_each template")
	}
	// Without the flag, a tool is mutating in both contexts.
	plain := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "plain"}}
	if !isMutatingNodeCtx(plain, "", nil, true) {
		t.Error("plain tool must stay mutating even on a fan_out_each template")
	}
}

func TestIsMutatingNode_SubbotDefaultMutating(t *testing.T) {
	// A subbot with no isolation assertion may run a child that touches the
	// shared workspace, so it is conservatively mutating.
	if !isMutatingNode(&ir.SubbotNode{BaseNode: ir.BaseNode{ID: "sb"}}) {
		t.Error("subbot node must be classified mutating by default")
	}
}

func TestIsMutatingNode_SubbotIsolatedOptOut(t *testing.T) {
	// `isolated:` asserts the child confines writes to its own run store /
	// worktree — the mirror of an agent's Readonly — so it is not mutating.
	n := &ir.SubbotNode{BaseNode: ir.BaseNode{ID: "sb"}, Isolated: true}
	if isMutatingNode(n) {
		t.Error("isolated subbot must not be classified mutating")
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

// TestIsMutatingNode_ClawReadOnlyToolsAreReadOnly pins the claw half of
// readOnlyTools. The names a claw node MUST use (C135 rejects the phantom
// ones) have to classify the same way, or renaming `list_files` → `glob` in a
// read-only reviewer silently tightens parallel-branch admission — a
// behaviour change for a rename that alters nothing about what the node does.
func TestIsMutatingNode_ClawReadOnlyToolsAreReadOnly(t *testing.T) {
	n := &ir.AgentNode{
		BaseNode: ir.BaseNode{ID: "a"},
		Tools:    []string{"read_file", "glob", "grep", "web_fetch"},
	}
	if isMutatingNode(n) {
		t.Error("claw's read-only trio (glob/grep/read_file, plus web_fetch) must not be classified as mutating")
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
	// With no backend signal (unit-test stub), preserve the model-only default.
	n := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}}
	if isMutatingNode(n) {
		t.Error("agent with no tools and no CLI backend should not be mutating")
	}
}

func TestIsMutatingNode_CLIBackendWithNoTools(t *testing.T) {
	for _, backend := range []string{"codex", "claude_code", "kimi", "grok", "custom_cli"} {
		n := &ir.AgentNode{
			BaseNode:  ir.BaseNode{ID: "a"},
			LLMFields: ir.LLMFields{Backend: backend},
		}
		if !isMutatingNode(n) {
			t.Errorf("unrestricted %s agent must be classified mutating", backend)
		}
	}
}

type fixedBackendResolver string

func (r fixedBackendResolver) EffectiveBackendName(ir.Node) string { return string(r) }

func TestIsMutatingNode_EffectiveBackendAndFullAccess(t *testing.T) {
	n := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}}
	if !isMutatingNodeWithBackend(n, "", fixedBackendResolver("codex")) {
		t.Error("agent resolved to unrestricted codex must be mutating")
	}
	if isMutatingNodeWithBackend(n, "codex", fixedBackendResolver("claw")) {
		t.Error("effective claw launch override must win over the workflow default")
	}
	full := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "full"}, LLMFields: ir.LLMFields{FullAccess: true}}
	if !isMutatingNode(full) {
		t.Error("full_access agent must be mutating")
	}
	locked := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "locked"},
		LLMFields: ir.LLMFields{Readonly: true, FullAccess: true, Backend: "codex"},
	}
	if isMutatingNode(locked) {
		t.Error("readonly must win over conflicting full_access")
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
			"router": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"a":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"done_a": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done_a"}},
			"done_b": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done_b"}},
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

// A fan-out target that is ALSO fed from outside the fan-out — the mono/dual
// topology review-pr and evolve ship, where a condition router reaches the
// same reviewer directly or through the fan_out_all — is not where the
// branches reconverge. Only predecessors inside the fan-out count.
func TestFindConvergencePoint_TargetFedFromOutsideTheFanOutIsNotTheCollector(t *testing.T) {
	wf := &ir.Workflow{
		Name: "t",
		Nodes: map[string]ir.Node{
			"topology": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "topology"}, RouterMode: ir.RouterCondition},
			"fan":      &ir.RouterNode{BaseNode: ir.BaseNode{ID: "fan"}, RouterMode: ir.RouterFanOutAll},
			"a":        &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b":        &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"merge":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "merge"}, AwaitMode: ir.AwaitBestEffort},
			"done":     &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "topology", To: "fan", ExpressionSrc: "vars.dual"},
			{From: "topology", To: "a", IsElse: true},
			{From: "fan", To: "a"},
			{From: "fan", To: "b"},
			{From: "a", To: "merge"},
			{From: "b", To: "merge"},
			{From: "merge", To: "done"},
		},
	}
	e := &Engine{workflow: wf}
	got := e.findConvergencePoint("fan", []*ir.Edge{
		{From: "fan", To: "a"},
		{From: "fan", To: "b"},
	})
	if got != "merge" {
		t.Errorf("expected convergence=merge, got %q", got)
	}
}

// Same class, fan_out_each: a template head reachable from the trunk as
// well must not be elected — every item replay would stop before running.
func TestFindConvergencePoint_TemplateHeadFedFromOutsideTheFanOutIsNotTheCollector(t *testing.T) {
	wf := &ir.Workflow{
		Name: "t",
		Nodes: map[string]ir.Node{
			"entry":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"dispatch": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "dispatch"}, RouterMode: ir.RouterFanOutEach},
			"work":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "work"}},
			"collect":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "collect"}, AwaitMode: ir.AwaitWaitAll},
			"done":     &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "dispatch", Condition: "many"},
			{From: "entry", To: "work", Condition: "many", Negated: true},
			{From: "dispatch", To: "work"},
			{From: "work", To: "collect"},
			{From: "collect", To: "done"},
		},
	}
	e := &Engine{workflow: wf}
	if got := e.findConvergencePoint("dispatch", []*ir.Edge{{From: "dispatch", To: "work"}}); got != "collect" {
		t.Errorf("expected convergence=collect, got %q", got)
	}
}

// A trunk edge bypassing the fan-out into the node BELOW the template head
// (`plan -> collect else`, the no-items case) is what makes `collect` the
// implicit collector of a linear template: the exemption for outside
// predecessors applies to branch heads only.
func TestFindConvergencePoint_TrunkBypassBelowTheHeadElectsTheImplicitCollector(t *testing.T) {
	wf := &ir.Workflow{
		Name: "t",
		Nodes: map[string]ir.Node{
			"plan":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "plan"}},
			"dispatch": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "dispatch"}, RouterMode: ir.RouterFanOutEach},
			"work":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "work"}},
			"collect":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "collect"}},
			"done":     &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "plan", To: "dispatch", Condition: "has_items"},
			{From: "plan", To: "collect", Condition: "has_items", Negated: true},
			{From: "dispatch", To: "work"},
			{From: "work", To: "collect"},
			{From: "collect", To: "done"},
		},
	}
	e := &Engine{workflow: wf}
	if got := e.findConvergencePoint("dispatch", []*ir.Edge{{From: "dispatch", To: "work"}}); got != "collect" {
		t.Errorf("expected convergence=collect, got %q", got)
	}
}

// A target fed by a SIBLING branch is a genuine reconvergence inside the
// fan-out and stays the collector.
func TestFindConvergencePoint_TargetFedBySiblingBranchStaysTheCollector(t *testing.T) {
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
			{From: "b", To: "a"},
			{From: "a", To: "done"},
		},
	}
	e := &Engine{workflow: wf}
	if got := e.findConvergencePoint("router", []*ir.Edge{{From: "router", To: "a"}, {From: "router", To: "b"}}); got != "a" {
		t.Errorf("expected convergence=a, got %q", got)
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
	if !e.branchContainsMutation("a", "global_join", false) {
		t.Error("BFS must catch mutation after an intermediate join, before the global convergence")
	}
	// Branch B has no mutation up to global_join.
	if e.branchContainsMutation("b", "global_join", false) {
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
	if e.branchContainsMutation("a", "", false) {
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

func TestValidateWorkspaceSafety_RejectsParallelUnrestrictedCodexAgents(t *testing.T) {
	wf := &ir.Workflow{
		Name:           "t",
		DefaultBackend: "codex",
		Nodes: map[string]ir.Node{
			"router":  &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"agent_a": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "agent_a"}},
			"agent_b": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "agent_b"}},
			"join":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "join"}, AwaitMode: ir.AwaitWaitAll},
			"done":    &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "router", To: "agent_a"},
			{From: "router", To: "agent_b"},
			{From: "agent_a", To: "join"},
			{From: "agent_b", To: "join"},
			{From: "join", To: "done"},
		},
	}
	e := &Engine{workflow: wf}
	if err := e.validateWorkspaceSafety("router", []*ir.Edge{
		{From: "router", To: "agent_a"},
		{From: "router", To: "agent_b"},
	}); err == nil {
		t.Fatal("parallel unrestricted codex agents must be rejected as mutating")
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
