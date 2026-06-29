package unparse_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// TestPermissionRoundTrip exercises parse → unparse → re-parse → re-compile on
// a workflow that uses the permission gate at every supported site: the scalar
// mode + allow/ask/deny rule lists at workflow level, and a per-node permission
// mode override on an agent, a judge, and a tool node. Unparse must emit the
// scalar mode as a bareword (no quotes, like compress/worktree) and the rule lists as
// quoted-string arrays (like capabilities/hosts), and the re-compiled IR must
// preserve every value verbatim.
func TestPermissionRoundTrip(t *testing.T) {
	src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  permission: deny

judge gate:
  model: "test-model"
  output: empty
  permission: ask

tool ship:
  command: "true"
  output: empty
  permission: off

workflow minimal:
  entry: start
  permission: ask
  allow: ["Read(**)"]
  ask: ["Bash(go build:*)"]
  deny: ["Bash(rm:*)"]
  start -> gate
  gate -> ship
  ship -> done
`
	pr1 := parser.Parse("permission.bot", src)
	for _, d := range pr1.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("original parse error: %s", d.Error())
		}
	}
	unparsed := unparse.Unparse(pr1.File)

	// Scalar modes emit as barewords; rule lists emit as quoted arrays.
	for _, want := range []string{
		"permission: ask",
		"permission: deny",
		"permission: off",
		`allow: ["Read(**)"]`,
		`ask: ["Bash(go build:*)"]`,
		`deny: ["Bash(rm:*)"]`,
	} {
		if !strings.Contains(unparsed, want) {
			t.Fatalf("unparse missing %q:\n%s", want, unparsed)
		}
	}

	pr2 := parser.Parse("permission.bot.roundtrip", unparsed)
	for _, d := range pr2.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("re-parse error: %s\nUnparsed:\n%s", d.Error(), unparsed)
		}
	}
	cr2 := ir.Compile(pr2.File)
	for _, d := range cr2.Diagnostics {
		if d.Severity == ir.SeverityError {
			t.Fatalf("re-compile error: %s\nUnparsed:\n%s", d.Error(), unparsed)
		}
	}
	w := cr2.Workflow
	if w == nil {
		t.Fatal("re-compile returned nil workflow")
	}
	if w.Permission != "ask" {
		t.Errorf("roundtrip workflow.Permission = %q, want ask", w.Permission)
	}
	if got := w.PermissionAllow; len(got) != 1 || got[0] != "Read(**)" {
		t.Errorf("roundtrip workflow.PermissionAllow = %v, want [Read(**)]", got)
	}
	if got := w.PermissionAsk; len(got) != 1 || got[0] != "Bash(go build:*)" {
		t.Errorf("roundtrip workflow.PermissionAsk = %v, want [Bash(go build:*)]", got)
	}
	if got := w.PermissionDeny; len(got) != 1 || got[0] != "Bash(rm:*)" {
		t.Errorf("roundtrip workflow.PermissionDeny = %v, want [Bash(rm:*)]", got)
	}
	if a, ok := w.Nodes["start"].(*ir.AgentNode); !ok || a.Permission != "deny" {
		t.Errorf("roundtrip start agent.Permission = %q, want deny", agentPermission(w.Nodes["start"]))
	}
	if j, ok := w.Nodes["gate"].(*ir.JudgeNode); !ok || j.Permission != "ask" {
		t.Errorf("roundtrip gate judge.Permission = %q, want ask", judgePermission(w.Nodes["gate"]))
	}
	if tn, ok := w.Nodes["ship"].(*ir.ToolNode); !ok || tn.Permission != "off" {
		t.Errorf("roundtrip ship tool.Permission = %q, want off", toolPermission(w.Nodes["ship"]))
	}
}

func agentPermission(n ir.Node) string {
	if a, ok := n.(*ir.AgentNode); ok {
		return a.Permission
	}
	return "<not-agent>"
}

func judgePermission(n ir.Node) string {
	if j, ok := n.(*ir.JudgeNode); ok {
		return j.Permission
	}
	return "<not-judge>"
}

func toolPermission(n ir.Node) string {
	if t, ok := n.(*ir.ToolNode); ok {
		return t.Permission
	}
	return "<not-tool>"
}
