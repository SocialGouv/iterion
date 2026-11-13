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

// TestTheAcceptedSpellingsAreExactlyOnOffAndInherit closes two holes an
// adversarial round found by mutation: widening CodemodeValues with a third
// word left the suite green — and a third word is not merely accepted, it
// resolves to OFF, which is the silent misreading the string form exists to
// prevent — and dropping the trim from normCodemode left the validator's own
// documented ` on ` example unproven.
//
// It asserts the predicate in BOTH directions, because `Valid` alone cannot
// see a value that is accepted and then read as something else: every
// accepted non-empty spelling must ALSO resolve to a decided on/off.
func TestTheAcceptedSpellingsAreExactlyOnOffAndInherit(t *testing.T) {
	accepted := map[string]bool{
		"":      true, // inherit
		"on":    true,
		"off":   true,
		"ON":    true, // the compiler lowercases before judging
		"Off":   true,
		" on ":  true, // and trims: the doc comment's own example
		"\toff": true,
	}
	for _, refused := range []string{"yes", "no", "true", "false", "1", "0", "enabled", "auto", "onn", "o n"} {
		accepted[refused] = false
	}
	for value, want := range accepted {
		if got := ValidCodemode(value); got != want {
			t.Errorf("ValidCodemode(%q) = %v, want %v", value, got, want)
		}
		if !want {
			continue
		}
		if value == "" {
			if p := ResolveCodemode(value, ""); p.Source != CodemodeFromDefault {
				t.Errorf("%q is the ABSENCE of a declaration and must inherit, got %+v", value, p)
			}
			continue
		}
		// An accepted spelling must be DECIDED, never silently read as off:
		// the node's rung has to answer, and the answer has to match the word.
		p := ResolveCodemode(value, "")
		if p.Source != CodemodeFromNode {
			t.Errorf("%q did not decide at the node rung: %+v", value, p)
		}
		if wantOn := strings.Contains(strings.ToLower(value), "on"); p.On != wantOn {
			t.Errorf("%q resolved On=%v, want %v — an accepted spelling the resolver reads as something else is worse than a refused one", value, p.On, wantOn)
		}
	}
}
