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

// TestCompileSandboxBlockForm exercises the full block-form parser
// end-to-end: parser → AST → IR. Covers image, env, mounts, user,
// post_create, workspace_folder, and the nested network block.
func TestCompileSandboxBlockForm(t *testing.T) {
	src := `
schema empty:
  ok: bool

prompt sys:
  hi

prompt usr:
  hi

agent start:
  model: "test-model"
  input: empty
  output: empty
  system: sys
  user: usr

workflow refactor:
  sandbox:
    image: "alpine:3"
    user: "node"
    workspace_folder: "/workspace"
    post_create: "npm install"
    env:
      KEY1: "v1"
      KEY2: "v2"
    mounts: ["type=bind,source=/a,target=/b"]
    network:
      mode: denylist
      preset: "iterion-default"
      rules: ["**", "!**.evil.site"]
  entry: start
  start -> done
`
	w := mustCompile(t, src)
	sb := w.Sandbox
	if sb == nil {
		t.Fatal("workflow.Sandbox is nil")
	}
	if sb.Mode != "inline" {
		t.Errorf("Mode = %q, want inline", sb.Mode)
	}
	if sb.Image != "alpine:3" {
		t.Errorf("Image = %q", sb.Image)
	}
	if sb.User != "node" {
		t.Errorf("User = %q", sb.User)
	}
	if sb.WorkspaceFolder != "/workspace" {
		t.Errorf("WorkspaceFolder = %q", sb.WorkspaceFolder)
	}
	if sb.PostCreate != "npm install" {
		t.Errorf("PostCreate = %q", sb.PostCreate)
	}
	if sb.Env["KEY1"] != "v1" || sb.Env["KEY2"] != "v2" {
		t.Errorf("Env = %v", sb.Env)
	}
	if len(sb.Mounts) != 1 || sb.Mounts[0] != "type=bind,source=/a,target=/b" {
		t.Errorf("Mounts = %v", sb.Mounts)
	}
	if sb.Network == nil {
		t.Fatal("Network is nil")
	}
	if sb.Network.Mode != "denylist" {
		t.Errorf("Network.Mode = %q", sb.Network.Mode)
	}
	if sb.Network.Preset != "iterion-default" {
		t.Errorf("Network.Preset = %q", sb.Network.Preset)
	}
	wantRules := []string{"**", "!**.evil.site"}
	if len(sb.Network.Rules) != len(wantRules) {
		t.Errorf("Network.Rules = %v, want %v", sb.Network.Rules, wantRules)
	} else {
		for i, want := range wantRules {
			if sb.Network.Rules[i] != want {
				t.Errorf("Network.Rules[%d] = %q, want %q", i, sb.Network.Rules[i], want)
			}
		}
	}
}

// TestCompileSandboxInlineRequiresImage verifies the diagnostic
// fires when a workflow declares mode=inline (or omits the mode in
// block form) without setting an image.
func TestCompileSandboxInlineRequiresImage(t *testing.T) {
	src := `
schema empty:
  ok: bool

prompt sys:
  hi

prompt usr:
  hi

agent start:
  model: "test-model"
  input: empty
  output: empty
  system: sys
  user: usr

workflow bad:
  sandbox:
    user: "node"
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
		t.Fatalf("expected DiagInvalidSandboxMode for inline-without-image, got %+v", r.Diagnostics)
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
