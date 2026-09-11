package ir

import (
	"strings"
	"testing"
)

func TestCapabilities_KnownAccepted(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  hi.

prompt usr:
  go.

agent po:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr
  capabilities: [board.create, board.move, board.read]

workflow w:
  entry: po
  po -> done
`
	r := compileFile(t, src)
	for _, d := range r.Diagnostics {
		if d.Severity == SeverityError {
			t.Fatalf("unexpected error: %s", d.Error())
		}
		if d.Code == DiagUnknownCapability {
			t.Fatalf("unexpected unknown-capability warning: %s", d.Error())
		}
	}
	if got := strings.Join(r.Workflow.Nodes["po"].(*AgentNode).Capabilities, ","); got != "board.create,board.move,board.read" {
		t.Fatalf("Capabilities propagation broken: %q", got)
	}
}

func TestCapabilities_WatchKnownAccepted(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  hi.

prompt usr:
  go.

agent po:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr
  capabilities: [watch.subscribe, watch.unsubscribe]

workflow w:
  entry: po
  po -> done
`
	r := compileFile(t, src)
	for _, d := range r.Diagnostics {
		if d.Severity == SeverityError {
			t.Fatalf("unexpected error: %s", d.Error())
		}
		if d.Code == DiagUnknownCapability {
			t.Fatalf("watch.* should be a known capability, got: %s", d.Error())
		}
	}
}

func TestCapabilities_RunsReadKnownAccepted(t *testing.T) {
	src := `
schema s:
  ok: bool
prompt sys:
  hi.
prompt usr:
  go.
agent po:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr
  capabilities: [runs.read]
workflow w:
  entry: po
  po -> done
`
	r := compileFile(t, src)
	for _, d := range r.Diagnostics {
		if d.Severity == SeverityError || d.Code == DiagUnknownCapability {
			t.Fatalf("runs.read should compile as a known capability: %s", d.Error())
		}
	}
}

func TestCapabilities_UnknownEmitsWarning(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  hi.

prompt usr:
  go.

agent po:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr
  capabilities: [board.create, runs.dispatch]

workflow w:
  entry: po
  po -> done
`
	r := compileFile(t, src)
	for _, d := range r.Diagnostics {
		if d.Severity == SeverityError {
			t.Fatalf("unexpected error: %s", d.Error())
		}
	}
	var sawWarn bool
	for _, d := range r.Diagnostics {
		if d.Code == DiagUnknownCapability && d.NodeID == "po" && strings.Contains(d.Message, "runs.dispatch") {
			sawWarn = true
		}
	}
	if !sawWarn {
		t.Fatalf("expected C080 warning for runs.dispatch on po; diags=%+v", r.Diagnostics)
	}
}

func TestCapabilities_MalformedEmitsError(t *testing.T) {
	// `Board.Create` (uppercase) doesn't match the lowercase shape.
	// We test the IR validator directly to bypass parser-level keyword constraints.
	w := &Workflow{
		Name: "w",
		Nodes: map[string]Node{
			"po": &AgentNode{
				BaseNode:     BaseNode{ID: "po"},
				Capabilities: []string{"Bad.Cap"},
			},
		},
	}
	c := &compiler{}
	c.validateCapabilities(w)
	var sawErr bool
	for _, d := range c.diags {
		if d.Code == DiagMalformedCapability && d.NodeID == "po" {
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatalf("expected C081 error for Bad.Cap; diags=%+v", c.diags)
	}
}
