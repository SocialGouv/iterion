package ir

import (
	"strings"
	"testing"
)

// TestCompileCodemode carries `codemode:` from the parser to the IR at the
// workflow level and on BOTH LLM node kinds, and reads it through the
// interface a consumer will hold rather than through the concrete type.
//
// Both halves are deliberate. The three AST→IR literal lines in compile.go
// are censused by nothing, and `ir.SameProgram` cannot see a missing one
// because both sides lose the field identically — so the judge arm is the
// classic silent loss: an agent-only test passes while every judge node reads
// "". And a test that built an `AgentNode{Codemode: "on"}` by hand would pass
// over a parser that never read the key, an encoder that dropped it and a
// compiler that left it behind: it starts from `.bot` TEXT for that reason.
//
// `off` must not collapse to "" either, which is the twin of the trap
// ambient_context's test names: "" means INHERIT, and a node that says `off`
// against a workflow that says `on` is the one shape a resolver has to get
// right.
func TestCompileCodemode(t *testing.T) {
	src := `
agent a:
  model: "m"
  codemode: on

judge j:
  model: "m"
  codemode: off

agent c:
  model: "m"

workflow w:
  entry: a
  codemode: on
  a -> j
  j -> c
  c -> done
`
	w := mustCompile(t, src)
	if w.Codemode != "on" {
		t.Errorf("workflow.Codemode = %q, want on", w.Codemode)
	}
	// Through LLMNode, not through *AgentNode: the accessor is the one site
	// the compiler itself holds (`var _ LLMNode = (*JudgeNode)(nil)`), and it
	// is what a consumer will call.
	for _, tc := range []struct{ node, want string }{
		{"a", "on"},
		{"j", "off"},
		{"c", ""},
	} {
		n, ok := w.Nodes[tc.node].(LLMNode)
		if !ok {
			t.Fatalf("node %q is not an LLMNode", tc.node)
		}
		if got := n.GetCodemode(); got != tc.want {
			t.Errorf("node %q GetCodemode() = %q, want %q", tc.node, got, tc.want)
		}
	}
	// "" and "off" are different answers: one inherits the workflow's `on`,
	// the other refuses it. A carry that normalised either way would make the
	// two indistinguishable here.
	if a, c := w.Nodes["a"].(LLMNode).GetCodemode(), w.Nodes["c"].(LLMNode).GetCodemode(); a == c {
		t.Errorf("an explicit `on` and an absent value both read %q: the carry collapsed them", a)
	}
}

// TestCodemodeReachesTheIRDecided holds what the IR field's comment claims.
// The compiler ACCEPTS `ON` and a padded ` on ` (it lowercases and trims
// before judging, as auto_memory does), so a carry of the author's raw text
// would hand a consumer `"ON"` and let `== "on"` read a node the compiler
// told its author was on as off. The AST keeps the raw text — that is what
// round-trips — and the IR keeps the decided value.
func TestCodemodeReachesTheIRDecided(t *testing.T) {
	w := mustCompile(t, `
agent a:
  model: "m"
  codemode: ON

judge j:
  model: "m"
  codemode: Off

workflow w:
  entry: a
  codemode: On
  a -> j
  j -> done
`)
	if w.Codemode != "on" {
		t.Errorf("workflow.Codemode = %q, want the decided on", w.Codemode)
	}
	if got := w.Nodes["a"].(LLMNode).GetCodemode(); got != "on" {
		t.Errorf("agent GetCodemode() = %q, want the decided on", got)
	}
	if got := w.Nodes["j"].(LLMNode).GetCodemode(); got != "off" {
		t.Errorf("judge GetCodemode() = %q, want the decided off", got)
	}
}

// TestCodemodeSaysNothingServesItYet is the honesty clause's own witness: the
// attribute exists, no build reads it, and the compiler says so on an
// explicit `on`.
//
// Counted PER SURFACE, not in one bucket. The catalogue entry, `docs/dsl.md`
// and `docs/references/diagnostics.md` all promise the warning on an agent, a
// judge AND a workflow; a one-bucket count let a mutant that skipped judges
// entirely (`|| kind != "agent"`) keep the whole perimeter green — measured.
// The fourth node declares nothing, and its zero is what holds "a workflow
// default does not warn per node" apart from the three real emissions.
func TestCodemodeSaysNothingServesItYet(t *testing.T) {
	r := compileFile(t, `
agent a:
  model: "m"
  codemode: on

judge j:
  model: "m"
  codemode: on

agent q:
  model: "m"
  codemode: off

agent silent:
  model: "m"

workflow w:
  entry: a
  codemode: on
  a -> j
  j -> q
  q -> silent
  silent -> done
`)
	// Keyed on the node id the diagnostic carries; the workflow's is empty.
	seen := map[string]int{}
	var invalid int
	for _, d := range r.Diagnostics {
		switch d.Code {
		case DiagCodemodeNoConsumer:
			seen[d.NodeID]++
			if !strings.Contains(d.Message, "codemode: on") {
				t.Errorf("C338 does not name what it is about: %q", d.Message)
			}
		case DiagInvalidCodemode:
			invalid++
		}
	}
	for _, want := range []struct {
		surface string
		n       int
	}{
		{"", 1},       // the workflow's own declaration
		{"a", 1},      // the agent
		{"j", 1},      // the judge — the surface a mutant could skip in silence
		{"q", 0},      // an explicit `off` says no, and is not warned about
		{"silent", 0}, // inheriting the workflow's `on` is not a declaration
	} {
		if got := seen[want.surface]; got != want.n {
			label := want.surface
			if label == "" {
				label = "the workflow"
			}
			t.Errorf("C338 on %s: %d time(s), want %d (all: %v)", label, got, want.n, seen)
		}
	}
	if invalid != 0 {
		t.Errorf("valid values produced %d invalid-value diagnostic(s)", invalid)
	}
}

// TestCodemodeWorkflowDefaultWarnsOnce holds the WORKFLOW arm of the honesty
// clause, which nothing else does: a review bench found that the whole
// `} else if norm(w.Codemode) == "on"` branch could be deleted, or lose its
// property name, or start warning on every inheriting node, and the gate
// stayed green. One declared line is one warning — and the warning sits on
// the WORKFLOW, which is where the author wrote it.
func TestCodemodeWorkflowDefaultWarnsOnce(t *testing.T) {
	r := compileFile(t, `
agent a:
  model: "m"

agent b:
  model: "m"

workflow w:
  entry: a
  codemode: on
  a -> b
  b -> done
`)
	if got := countCode(r, DiagCodemodeNoConsumer); got != 1 {
		t.Errorf("C338 emitted %d time(s) for one workflow declaration and two inheriting nodes, want 1: %+v",
			got, r.Diagnostics)
	}
	for _, d := range r.Diagnostics {
		if d.Code != DiagCodemodeNoConsumer {
			continue
		}
		if d.NodeID != "" || d.Property != "codemode" {
			t.Errorf("the workflow warning is not positioned on the workflow's own property: node=%q property=%q",
				d.NodeID, d.Property)
		}
	}
}

// TestInvalidCodemodeIsAnError pins the value contract on every surface that
// can carry it. The workflow case is not a duplicate of the node cases: its
// parser arm is a different shape (it peeks, so it consumes its own keyword
// and its own newlines), and a copy-pasted node arm there loses a token.
func TestInvalidCodemodeIsAnError(t *testing.T) {
	for name, src := range map[string]string{
		"on an agent": `
agent a:
  model: "m"
  codemode: sometimes

workflow w:
  entry: a
  a -> done
`,
		"on a judge": `
judge j:
  model: "m"
  codemode: sometimes

workflow w:
  entry: j
  j -> done
`,
		"on the workflow": `
agent a:
  model: "m"

workflow w:
  entry: a
  codemode: sometimes
  a -> done
`,
	} {
		t.Run(name, func(t *testing.T) {
			r := compileFile(t, src)
			found := false
			for _, d := range r.Diagnostics {
				if d.Code == DiagInvalidCodemode {
					found = true
					if !strings.Contains(d.Message, "sometimes") {
						t.Errorf("the diagnostic does not quote the value it refused: %q", d.Message)
					}
				}
			}
			if !found {
				t.Errorf("no %s for an invalid value; got %+v", DiagInvalidCodemode, r.Diagnostics)
			}
		})
	}
}
