package ir

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

const supervisorWF = `
prompt watchdog_policy:
  Intervene if the implementer edits files outside src/.

supervisor watchdog:
  watches: [implement]
  model: "anthropic/claude-opus-4-8"
  system: watchdog_policy
  cooldown: "45s"
  max_evals: 12

agent implement:
  model: "anthropic/claude-opus-4-8"

workflow main:
  entry: implement
  implement -> done
`

func TestCompileSupervisorDeclaration(t *testing.T) {
	pr := parser.Parse("t.bot", supervisorWF)
	if pr.File == nil {
		t.Fatalf("parse returned nil: %v", pr.Diagnostics)
	}
	res := Compile(pr.File)
	for _, d := range res.Diagnostics {
		if d.Severity == SeverityError {
			t.Fatalf("unexpected compile error: %s %s", d.Code, d.Message)
		}
	}
	w := res.Workflow
	if w == nil {
		t.Fatal("nil workflow")
	}
	if len(w.Supervisors) != 1 {
		t.Fatalf("got %d supervisors; want 1", len(w.Supervisors))
	}
	s := w.Supervisors[0]
	if s.Name != "watchdog" {
		t.Errorf("name=%q; want watchdog", s.Name)
	}
	if len(s.Watches) != 1 || s.Watches[0] != "implement" {
		t.Errorf("watches=%v; want [implement]", s.Watches)
	}
	if s.Model != "anthropic/claude-opus-4-8" {
		t.Errorf("model=%q", s.Model)
	}
	if s.System != "watchdog_policy" {
		t.Errorf("system=%q; want watchdog_policy", s.System)
	}
	if s.Cooldown != 45*time.Second {
		t.Errorf("cooldown=%v; want 45s", s.Cooldown)
	}
	if s.MaxEvals != 12 {
		t.Errorf("max_evals=%d; want 12", s.MaxEvals)
	}
}

// A supervisor watching a non-existent / non-agent node warns (C190) but
// does not fail compilation — supervision is an enhancement.
func TestSupervisorUnknownWatchedNodeWarns(t *testing.T) {
	src := `
supervisor wd:
  watches: [nope]

agent implement:
  model: "anthropic/claude-opus-4-8"

workflow main:
  entry: implement
  implement -> done
`
	pr := parser.Parse("t.bot", src)
	res := Compile(pr.File)
	var sawWarn bool
	for _, d := range res.Diagnostics {
		if d.Code == DiagUnknownWatchedNode {
			sawWarn = true
			if d.Severity == SeverityError {
				t.Fatalf("C190 must be a warning, got error")
			}
		}
	}
	if !sawWarn {
		t.Fatalf("expected C190 unknown-watched-node warning; diags=%v", res.Diagnostics)
	}
	if res.Workflow == nil {
		t.Fatal("workflow must still compile despite the warning")
	}
}

// A supervisor watching a JUDGE node is legitimate (a reviewer is a
// judge; both kinds execute through the same model executor and drain
// steering identically) — no C190.
func TestSupervisorWatchingJudgeNodeDoesNotWarn(t *testing.T) {
	src := `
supervisor pacer:
  watches: [review]

judge review:
  model: "anthropic/claude-opus-4-8"

workflow main:
  entry: review
  review -> done
`
	pr := parser.Parse("t.bot", src)
	res := Compile(pr.File)
	for _, d := range res.Diagnostics {
		if d.Code == DiagUnknownWatchedNode {
			t.Fatalf("C190 fired for a judge watch: %s", d.Message)
		}
	}
	if res.Workflow == nil {
		t.Fatal("workflow must compile")
	}
}
