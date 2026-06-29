package unparse_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// TestCompressRoundTrip exercises parse → unparse → re-parse → re-compile
// on a workflow that uses compress at every supported site. Unparse must
// emit each value as a bareword (no quotes, like worktree), and the
// re-compiled IR must preserve every Compress verbatim.
func TestCompressRoundTrip(t *testing.T) {
	src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  compress: ultra

judge gate:
  model: "test-model"
  output: empty
  compress: off

tool ship:
  command: "true"
  output: empty
  compress: on

workflow minimal:
  entry: start
  compress: on
  start -> gate
  gate -> ship
  ship -> done
`
	pr1 := parser.Parse("rtk.bot", src)
	for _, d := range pr1.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("original parse error: %s", d.Error())
		}
	}
	unparsed := unparse.Unparse(pr1.File)
	// Every site must emit `compress: <value>` as a bareword.
	for _, want := range []string{"compress: on", "compress: ultra", "compress: off"} {
		if !strings.Contains(unparsed, want) {
			t.Fatalf("unparse missing %q:\n%s", want, unparsed)
		}
	}

	pr2 := parser.Parse("rtk.bot.roundtrip", unparsed)
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
	if w.Compress != "on" {
		t.Errorf("roundtrip workflow.Compress = %q, want on", w.Compress)
	}
	if a, ok := w.Nodes["start"].(*ir.AgentNode); !ok || a.Compress != "ultra" {
		t.Errorf("roundtrip start agent.Compress = %q, want ultra", agentCompress(w.Nodes["start"]))
	}
	if j, ok := w.Nodes["gate"].(*ir.JudgeNode); !ok || j.Compress != "off" {
		t.Errorf("roundtrip gate judge.Compress = %q, want off", judgeCompress(w.Nodes["gate"]))
	}
	if tn, ok := w.Nodes["ship"].(*ir.ToolNode); !ok || tn.Compress != "on" {
		t.Errorf("roundtrip ship tool.Compress = %q, want on", toolCompress(w.Nodes["ship"]))
	}
}

func agentCompress(n ir.Node) string {
	if a, ok := n.(*ir.AgentNode); ok {
		return a.Compress
	}
	return "<not-agent>"
}

func judgeCompress(n ir.Node) string {
	if j, ok := n.(*ir.JudgeNode); ok {
		return j.Compress
	}
	return "<not-judge>"
}

func toolCompress(n ir.Node) string {
	if t, ok := n.(*ir.ToolNode); ok {
		return t.Compress
	}
	return "<not-tool>"
}
