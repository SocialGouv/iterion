package unparse_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// TestAutoMemoryRoundTrip exercises parse → unparse → re-parse → re-compile
// on a workflow using auto_memory at every supported site. Unparse must emit
// barewords (like compress/worktree), and the re-compiled IR must preserve
// every value verbatim — a dropped `off` would silently re-enable memory on a
// node that opted out.
func TestAutoMemoryRoundTrip(t *testing.T) {
	src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  auto_memory: on

judge gate:
  model: "test-model"
  output: empty
  auto_memory: off

workflow minimal:
  entry: start
  auto_memory: on
  start -> gate
  gate -> done
`
	pr1 := parser.Parse("am.bot", src)
	for _, d := range pr1.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("original parse error: %s", d.Error())
		}
	}
	unparsed := unparse.Unparse(pr1.File)
	for _, want := range []string{"auto_memory: on", "auto_memory: off"} {
		if !strings.Contains(unparsed, want) {
			t.Fatalf("unparse missing %q:\n%s", want, unparsed)
		}
	}

	pr2 := parser.Parse("am.bot.roundtrip", unparsed)
	for _, d := range pr2.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("re-parse error: %s\nUnparsed:\n%s", d.Error(), unparsed)
		}
	}
	res := ir.Compile(pr2.File)
	for _, d := range res.Diagnostics {
		if d.Severity == ir.SeverityError {
			t.Fatalf("re-compile error: %s\nUnparsed:\n%s", d.Error(), unparsed)
		}
	}
	w := res.Workflow
	if w.AutoMemory != "on" {
		t.Errorf("workflow.AutoMemory = %q, want on", w.AutoMemory)
	}
	if a := w.Nodes["start"].(*ir.AgentNode); a.AutoMemory != "on" {
		t.Errorf("agent.AutoMemory = %q, want on", a.AutoMemory)
	}
	if j := w.Nodes["gate"].(*ir.JudgeNode); j.AutoMemory != "off" {
		t.Errorf("judge.AutoMemory = %q, want off", j.AutoMemory)
	}
}
