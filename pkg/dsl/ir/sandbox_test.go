package ir

import (
	"testing"
)

func TestFromIdent(t *testing.T) {
	cases := []struct {
		in       string
		wantSpec *SandboxSpec
		wantOK   bool
	}{
		{"", nil, true},
		{"none", &SandboxSpec{Mode: "none"}, true},
		{"auto", &SandboxSpec{Mode: "auto"}, true},
		{"inline", nil, false}, // requires block body in Phase 0 parser
		{"garbage", nil, false},
		{"AUTO", nil, false}, // case-sensitive
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			spec, ok := FromIdent(c.in)
			if ok != c.wantOK {
				t.Fatalf("FromIdent(%q) ok = %v, want %v", c.in, ok, c.wantOK)
			}
			if c.wantSpec == nil {
				if spec != nil {
					t.Errorf("FromIdent(%q) spec = %+v, want nil", c.in, spec)
				}
				return
			}
			if spec == nil || spec.Mode != c.wantSpec.Mode {
				t.Errorf("FromIdent(%q) spec = %+v, want %+v", c.in, spec, c.wantSpec)
			}
		})
	}
}

func TestSandboxSpecIsActive(t *testing.T) {
	cases := []struct {
		in   *SandboxSpec
		want bool
	}{
		{nil, false},
		{&SandboxSpec{}, false},
		{&SandboxSpec{Mode: ""}, false},
		{&SandboxSpec{Mode: "none"}, false},
		{&SandboxSpec{Mode: "auto"}, true},
		{&SandboxSpec{Mode: "inline"}, true},
	}
	for _, c := range cases {
		if got := c.in.IsActive(); got != c.want {
			t.Errorf("(%+v).IsActive() = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestCompileSandboxRoundTrip exercises the full pipeline: parser → AST
// → IR for both a workflow-level sandbox declaration and node-level
// overrides. This catches regressions in the token, AST, parser, and
// compile.go wiring as a single end-to-end check.
func TestCompileSandboxRoundTrip(t *testing.T) {
	src := `
schema empty:
  ok: bool

prompt sys:
  You are a minimal agent.

prompt usr:
  Do something.

agent start:
  model: "test-model"
  input: empty
  output: empty
  system: sys
  user: usr
  sandbox: none

workflow minimal:
  sandbox: auto
  entry: start
  start -> done
`
	w := mustCompile(t, src)

	if w.Sandbox == nil {
		t.Fatal("workflow.Sandbox is nil; want auto")
	}
	if w.Sandbox.Mode != "auto" {
		t.Errorf("workflow.Sandbox.Mode = %q, want auto", w.Sandbox.Mode)
	}

	agent, ok := w.Nodes["start"].(*AgentNode)
	if !ok {
		t.Fatalf("start node = %T, want *AgentNode", w.Nodes["start"])
	}
	if agent.Sandbox == nil {
		t.Fatal("agent.Sandbox is nil; want none")
	}
	if agent.Sandbox.Mode != "none" {
		t.Errorf("agent.Sandbox.Mode = %q, want none", agent.Sandbox.Mode)
	}
}

// TestCompileSandboxInvalidIdent ensures C044 fires for unknown sandbox
// modes at the workflow scope.
func TestCompileSandboxInvalidIdent(t *testing.T) {
	src := `
schema empty:
  ok: bool

prompt sys:
  Hi.

prompt usr:
  Hi.

agent start:
  model: "test-model"
  input: empty
  output: empty
  system: sys
  user: usr

workflow bad:
  sandbox: bogus
  entry: start
  start -> done
`
	r := compileFile(t, src)
	found := false
	for _, d := range r.Diagnostics {
		if d.Code == DiagInvalidSandboxMode {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected DiagInvalidSandboxMode (C044), got %+v", r.Diagnostics)
	}
}
