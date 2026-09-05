package runtime

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestContainsClawNode_SeesFallbackRoutes: the sandbox's iterion
// bind-mount is decided ONCE, before the run, from static backend
// strings. A claw route reachable only through a `fallbacks:` block
// would otherwise get no in-container binary and die with
// `exec: iterion: not found` — at the worst possible moment, just as the
// primary's quota ran out and the chain is advancing.
func TestContainsClawNode_SeesFallbackRoutes(t *testing.T) {
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"a": &ir.AgentNode{
			BaseNode:  ir.BaseNode{ID: "a"},
			LLMFields: ir.LLMFields{Backend: "claude_code"},
			Fallbacks: []ir.Fallback{{Name: "api", Backend: "claw", Model: "openai/gpt-5.5"}},
		},
	}}
	if !containsClawNode(wf, nil) {
		t.Error("a claw route inside a fallbacks: block must still mount the iterion binary")
	}
}

// TestContainsClawNode_JudgeFallbackRoutesToo guards the half-wiring
// that is easy to ship: teaching the agent branch and forgetting the
// judge one.
func TestContainsClawNode_JudgeFallbackRoutesToo(t *testing.T) {
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"j": &ir.JudgeNode{
			BaseNode:  ir.BaseNode{ID: "j"},
			LLMFields: ir.LLMFields{Backend: "claude_code"},
			Fallbacks: []ir.Fallback{{Name: "api", Backend: "claw", Model: "openai/gpt-5.5"}},
		},
	}}
	if !containsClawNode(wf, nil) {
		t.Error("a judge's claw route must mount the binary too")
	}
}

// TestContainsClawNode_InheritingRouteAddsNothing: a route that leaves
// `backend:` empty inherits the node's, so it cannot introduce claw that
// the node did not already declare. Treating an empty route backend as
// claw (the way the node-level default does) would mount the binary for
// every chain-bearing workflow.
func TestContainsClawNode_InheritingRouteAddsNothing(t *testing.T) {
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"a": &ir.AgentNode{
			BaseNode:  ir.BaseNode{ID: "a"},
			LLMFields: ir.LLMFields{Backend: "claude_code"},
			Fallbacks: []ir.Fallback{{Name: "same", Model: "claude-sonnet-5"}},
		},
	}}
	if containsClawNode(wf, nil) {
		t.Error("a route with no backend of its own must not be read as claw")
	}
}

// TestWorkspaceSafety_ClawNodeWithCLIRouteIsMutating is the
// lost-write guard.
//
// A tools-less claw node is admitted as non-mutating because on claw an
// empty list means ZERO tools. But the same empty list on a CLI backend
// under bypassPermissions is the FULL native toolset — Write, Edit,
// Bash. Admission happens once, before the run, so a node that can
// chain-switch must be judged on its most permissive route: otherwise a
// fan_out_each over N items passes the parallel-safety check and then,
// the moment the primary's quota runs out, runs N write-capable agents
// on one worktree with every guard already behind it.
func TestWorkspaceSafety_ClawNodeWithCLIRouteIsMutating(t *testing.T) {
	readOnly := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "a"},
		LLMFields: ir.LLMFields{Backend: "claw"},
	}
	if isMutatingNodeCtx(readOnly, "", nil, false) {
		t.Fatal("precondition: a tools-less claw node is read-only")
	}

	withCLIRoute := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "a"},
		LLMFields: ir.LLMFields{Backend: "claw"},
		Fallbacks: []ir.Fallback{{Name: "cli", Backend: "claude_code", Model: "claude-opus-5"}},
	}
	if !isMutatingNodeCtx(withCLIRoute, "", nil, false) {
		t.Error("a tools-less claw node with a CLI route must be admitted as MUTATING — its empty tools list becomes the full native toolset the moment the chain falls through")
	}
}

// TestWorkspaceSafety_ToolsListDoesNotShelterCLIRoute: declaring a
// read-only `tools:` list does NOT make a claw→CLI route safe. Under
// bypassPermissions a CLI agent ignores the lowercase list entirely and
// carries the full native toolset, so the node gains Edit/Write on
// fall-through — while the admission decided on the claw reading has
// already let it run as one of N concurrent read-only branches.
func TestWorkspaceSafety_ToolsListDoesNotShelterCLIRoute(t *testing.T) {
	node := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "a"},
		LLMFields: ir.LLMFields{Backend: "claw"},
		Tools:     []string{"read_file"},
		Fallbacks: []ir.Fallback{{Name: "cli", Backend: "claude_code", Model: "claude-opus-5"}},
	}
	if !isMutatingNodeCtx(node, "", nil, false) {
		t.Error("a read-only tools list must not shelter a claw→CLI route from the mutation classifier")
	}
}

// TestWorkspaceSafety_ClawOnlyRouteStaysReadOnly keeps the pessimism
// narrow: a route that stays on claw changes nothing about what the node
// can do, so it must not cost the node its parallel-fan-out eligibility.
func TestWorkspaceSafety_ClawOnlyRouteStaysReadOnly(t *testing.T) {
	node := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "a"},
		LLMFields: ir.LLMFields{Backend: "claw"},
		Fallbacks: []ir.Fallback{{Name: "other", Backend: "claw", Model: "openai/gpt-5.5"}},
	}
	if isMutatingNodeCtx(node, "", nil, false) {
		t.Error("a claw→claw route must not make a read-only node mutating")
	}
}
