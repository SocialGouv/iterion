package unparse_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// The Verified Action quad (ADR-044) must survive an IR→.bot round-trip so
// `iterion resume` change-detection hashing stays correct.
func TestUnparseVerifiedAction_RoundTrip(t *testing.T) {
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

workflow w:
  entry: commit_changes
  commit_changes -> done
`
	res := parser.Parse("test.bot", src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", res.Diagnostics)
	}

	out := unparse.Unparse(res.File)
	for _, want := range []string{
		`goal: "advance HEAD by one commit"`,
		`postcondition: "git rev-parse HEAD"`,
		"policy: recover",
		"recovery:",
		"max_repair_attempts: 2",
		"max_agent_attempts: 1",
		`model: "anthropic/claude-sonnet-4-6"`,
		"agent_tools: [bash, read_file]",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("unparse output missing %q:\n%s", want, out)
		}
	}

	// Reparse: the quad must come back identical.
	res2 := parser.Parse("test.bot", out)
	if len(res2.Diagnostics) != 0 {
		t.Fatalf("reparse diagnostics: %+v\nsource:\n%s", res2.Diagnostics, out)
	}
	tn := res2.File.Tools[0]
	if tn.Goal != "advance HEAD by one commit" || tn.Postcondition != "git rev-parse HEAD" || tn.Policy != "recover" {
		t.Fatalf("quad not preserved: %+v", tn)
	}
	if tn.Recovery == nil || tn.Recovery.MaxRepairAttempts != 2 || tn.Recovery.MaxAgentAttempts != 1 {
		t.Fatalf("recovery not preserved: %+v", tn.Recovery)
	}
}

// parallel_safe must survive an IR→.bot round-trip so `iterion resume`
// change-detection hashing stays correct.
func TestUnparseParallelSafe_RoundTrip(t *testing.T) {
	src := `tool render_scene:
  command: "render --scene {{outputs.dispatch.item.id}}"
  parallel_safe: true

workflow w:
  entry: render_scene
  render_scene -> done
`
	res := parser.Parse("test.bot", src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", res.Diagnostics)
	}

	out := unparse.Unparse(res.File)
	if !strings.Contains(out, "parallel_safe: true") {
		t.Fatalf("unparse output missing parallel_safe:\n%s", out)
	}

	res2 := parser.Parse("test.bot", out)
	if len(res2.Diagnostics) != 0 {
		t.Fatalf("reparse diagnostics: %+v\nsource:\n%s", res2.Diagnostics, out)
	}
	if !res2.File.Tools[0].ParallelSafe {
		t.Fatal("parallel_safe not preserved across round-trip")
	}
}
