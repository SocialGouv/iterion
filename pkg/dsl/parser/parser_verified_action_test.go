package parser_test

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// The Verified Action quad (ADR-044) adds optional goal / postcondition /
// policy / recovery properties to tool nodes.
func TestParseVerifiedActionQuad(t *testing.T) {
	src := `tool commit_changes:
  command: "git commit -am wip"
  goal: "advance HEAD by one commit"
  postcondition: "git rev-parse HEAD"
  policy: recover
  recovery:
    max_repair_attempts: 2
    max_agent_attempts: 1
    model: "anthropic/claude-sonnet-4-6"
    agent_tools: [bash, read_file]
`
	res := parser.Parse("test.bot", src)
	assertNoDiags(t, res)

	if len(res.File.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(res.File.Tools))
	}
	tn := res.File.Tools[0]
	assertEq(t, "Goal", tn.Goal, "advance HEAD by one commit")
	assertEq(t, "Postcondition", tn.Postcondition, "git rev-parse HEAD")
	assertEq(t, "Policy", tn.Policy, "recover")
	if tn.Recovery == nil {
		t.Fatal("expected recovery block")
	}
	assertEq(t, "MaxRepairAttempts", tn.Recovery.MaxRepairAttempts, 2)
	assertEq(t, "MaxAgentAttempts", tn.Recovery.MaxAgentAttempts, 1)
	assertEq(t, "Model", tn.Recovery.Model, "anthropic/claude-sonnet-4-6")
	if len(tn.Recovery.AgentTools) != 2 || tn.Recovery.AgentTools[0] != "bash" {
		t.Fatalf("AgentTools = %v, want [bash read_file]", tn.Recovery.AgentTools)
	}
}

// parallel_safe is an opt-in bool on tool nodes that lets a mutating tool fan
// out in parallel (its concurrent replays write to disjoint targets).
func TestParseToolParallelSafe(t *testing.T) {
	src := `tool render_scene:
  command: "render --scene {{outputs.dispatch.item.id}}"
  parallel_safe: true
`
	res := parser.Parse("test.bot", src)
	assertNoDiags(t, res)
	if len(res.File.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(res.File.Tools))
	}
	if !res.File.Tools[0].ParallelSafe {
		t.Fatal("expected ParallelSafe = true")
	}

	// Absent the property, it defaults to false (safe, serialized-only fan-out).
	res2 := parser.Parse("test.bot", "tool plain:\n  command: \"echo ok\"\n")
	assertNoDiags(t, res2)
	if res2.File.Tools[0].ParallelSafe {
		t.Fatal("plain tool must default ParallelSafe = false")
	}
}

// A tool node with no quad fields still parses (backward compatible).
func TestParseToolNodeNoQuad(t *testing.T) {
	src := `tool plain:
  command: "echo ok"
`
	res := parser.Parse("test.bot", src)
	assertNoDiags(t, res)
	tn := res.File.Tools[0]
	if tn.Goal != "" || tn.Postcondition != "" || tn.Policy != "" || tn.Recovery != nil {
		t.Fatalf("plain tool node should carry no quad fields: %+v", tn)
	}
}

// An unknown property inside the recovery block is a diagnostic.
func TestParseRecoveryUnknownProperty(t *testing.T) {
	src := `tool t:
  command: "echo ok"
  postcondition: "true"
  policy: recover
  recovery:
    bogus_field: 3
`
	res := parser.Parse("test.bot", src)
	if !hasDiagCode(res, parser.DiagUnknownProperty) {
		t.Fatalf("expected DiagUnknownProperty for bogus recovery field, got %v", res.Diagnostics)
	}
}
