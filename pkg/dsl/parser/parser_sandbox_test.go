package parser_test

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// Focused unit coverage for parser_sandbox.go (extracted in 9f67d6fd).
// The file ships with zero dedicated tests; existing parser tests
// touch only the trivial short forms via examples/. The cases below
// drive the block-form helpers (parseSandboxBlock, parseSandboxProp,
// parseSandboxBuildBody, parseSandboxNetworkBody) plus the shared
// list/map helpers, including their diagnostic branches.

// agentWithSandbox wraps a sandbox snippet in the minimum agent
// scaffolding so the parser reaches parseSandboxBlock via the agent
// declaration. The returned source compiles cleanly when the sandbox
// body is well-formed.
func agentWithSandbox(body string) string {
	return `agent worker:
  model: "anthropic/claude-sonnet-4-6"
  input: in_s
  output: out_s
  system: sys
  user: usr
` + body + `
workflow flow:
  entry: worker
  worker -> done
`
}

func TestSandbox_ShortFormAuto(t *testing.T) {
	res := parser.ParseLenient("test.bot", agentWithSandbox("  sandbox: auto"))
	assertNoDiags(t, res)
	sb := res.File.Agents[0].Sandbox
	if sb == nil {
		t.Fatal("expected sandbox block")
	}
	assertEq(t, "Mode", sb.Mode, "auto")
	assertEq(t, "Image", sb.Image, "")
}

func TestSandbox_ShortFormNone(t *testing.T) {
	res := parser.ParseLenient("test.bot", agentWithSandbox("  sandbox: none"))
	assertNoDiags(t, res)
	assertEq(t, "Mode", res.File.Agents[0].Sandbox.Mode, "none")
}

func TestSandbox_BlockFormImageOnly(t *testing.T) {
	res := parser.ParseLenient("test.bot", agentWithSandbox(`  sandbox:
    image: "iterion-sandbox-slim:1.2.3"
    user: "iterion"
    workspace_folder: "/workspace"`))
	assertNoDiags(t, res)
	sb := res.File.Agents[0].Sandbox
	if sb == nil {
		t.Fatal("expected sandbox block")
	}
	// Block-form without explicit mode defaults to "inline" per the helper.
	assertEq(t, "Mode", sb.Mode, "inline")
	assertEq(t, "Image", sb.Image, "iterion-sandbox-slim:1.2.3")
	assertEq(t, "User", sb.User, "iterion")
	assertEq(t, "WorkspaceFolder", sb.WorkspaceFolder, "/workspace")
}

func TestSandbox_BlockFormBuild(t *testing.T) {
	res := parser.ParseLenient("test.bot", agentWithSandbox(`  sandbox:
    build:
      dockerfile: "Containerfile"
      context: ".devcontainer"
      args:
        BASE_IMAGE: "alpine:3.20"
        DEBUG: "1"`))
	assertNoDiags(t, res)
	sb := res.File.Agents[0].Sandbox
	if sb == nil || sb.Build == nil {
		t.Fatal("expected sandbox.build block")
	}
	assertEq(t, "Dockerfile", sb.Build.Dockerfile, "Containerfile")
	assertEq(t, "Context", sb.Build.Context, ".devcontainer")
	if got := sb.Build.Args["BASE_IMAGE"]; got != "alpine:3.20" {
		t.Errorf("Args[BASE_IMAGE]: got %q, want alpine:3.20", got)
	}
	if got := sb.Build.Args["DEBUG"]; got != "1" {
		t.Errorf("Args[DEBUG]: got %q, want 1", got)
	}
}

func TestSandbox_BlockFormNetwork(t *testing.T) {
	res := parser.ParseLenient("test.bot", agentWithSandbox(`  sandbox:
    image: "iterion-sandbox-slim:1.2.3"
    network:
      mode: allowlist
      preset: "iterion-default"
      rules: ["github.com", "*.npmjs.org", "!*.evil.site"]
      inherit: merge`))
	assertNoDiags(t, res)
	sb := res.File.Agents[0].Sandbox
	if sb == nil || sb.Network == nil {
		t.Fatal("expected sandbox.network block")
	}
	nb := sb.Network
	assertEq(t, "Mode", nb.Mode, "allowlist")
	assertEq(t, "Preset", nb.Preset, "iterion-default")
	assertEq(t, "Inherit", nb.Inherit, "merge")
	if len(nb.Rules) != 3 {
		t.Fatalf("expected 3 rules, got %d", len(nb.Rules))
	}
	if nb.Rules[0] != "github.com" || nb.Rules[1] != "*.npmjs.org" || nb.Rules[2] != "!*.evil.site" {
		t.Errorf("unexpected rules: %v", nb.Rules)
	}
}

func TestSandbox_HostStateNone(t *testing.T) {
	res := parser.ParseLenient("test.bot", agentWithSandbox(`  sandbox:
    image: "iterion-sandbox-slim:1.2.3"
    host_state: none`))
	assertNoDiags(t, res)
	assertEq(t, "HostState", res.File.Agents[0].Sandbox.HostState, "none")
}

func TestSandbox_EnvInlineAndBlock(t *testing.T) {
	// Inline form
	res := parser.ParseLenient("test.bot", agentWithSandbox(`  sandbox:
    image: "iterion-sandbox-slim:1.2.3"
    env: { FOO: "bar", BAZ: "qux" }`))
	assertNoDiags(t, res)
	env := res.File.Agents[0].Sandbox.Env
	if env["FOO"] != "bar" || env["BAZ"] != "qux" {
		t.Errorf("inline env: got %v, want FOO=bar, BAZ=qux", env)
	}

	// Block form
	res = parser.ParseLenient("test.bot", agentWithSandbox(`  sandbox:
    image: "iterion-sandbox-slim:1.2.3"
    env:
      FOO: "bar"
      BAZ: "qux"`))
	assertNoDiags(t, res)
	env = res.File.Agents[0].Sandbox.Env
	if env["FOO"] != "bar" || env["BAZ"] != "qux" {
		t.Errorf("block env: got %v, want FOO=bar, BAZ=qux", env)
	}
}

func TestSandbox_MountsList(t *testing.T) {
	res := parser.ParseLenient("test.bot", agentWithSandbox(`  sandbox:
    image: "iterion-sandbox-slim:1.2.3"
    mounts: ["type=bind,src=/host/cache,dst=/cache", "type=volume,src=nx-state,dst=/state"]`))
	assertNoDiags(t, res)
	mounts := res.File.Agents[0].Sandbox.Mounts
	if len(mounts) != 2 {
		t.Fatalf("expected 2 mounts, got %d", len(mounts))
	}
}

func TestSandbox_UnknownProperty(t *testing.T) {
	res := parser.ParseLenient("test.bot", agentWithSandbox(`  sandbox:
    image: "iterion-sandbox-slim:1.2.3"
    bogus: "x"`))
	if !hasDiagCode(res, parser.DiagUnknownProperty) {
		t.Fatalf("expected DiagUnknownProperty, got %v", res.Diagnostics)
	}
	// image must still be captured despite the bad property.
	if got := res.File.Agents[0].Sandbox.Image; got != "iterion-sandbox-slim:1.2.3" {
		t.Errorf("Image lost after error: got %q", got)
	}
}

func TestSandbox_BuildUnknownProperty(t *testing.T) {
	res := parser.ParseLenient("test.bot", agentWithSandbox(`  sandbox:
    build:
      dockerfile: "Containerfile"
      bogus_build_prop: "x"
      context: "."`))
	if !hasDiagCode(res, parser.DiagUnknownProperty) {
		t.Fatalf("expected DiagUnknownProperty in build block, got %v", res.Diagnostics)
	}
	bb := res.File.Agents[0].Sandbox.Build
	if bb == nil {
		t.Fatal("expected build block to survive recovery")
	}
	// Both surrounding props must be captured around the error.
	assertEq(t, "Dockerfile", bb.Dockerfile, "Containerfile")
	assertEq(t, "Context", bb.Context, ".")
}

func TestSandbox_NetworkUnknownProperty(t *testing.T) {
	res := parser.ParseLenient("test.bot", agentWithSandbox(`  sandbox:
    image: "iterion-sandbox-slim:1.2.3"
    network:
      mode: allowlist
      bogus_net_prop: "x"
      rules: ["github.com"]`))
	if !hasDiagCode(res, parser.DiagUnknownProperty) {
		t.Fatalf("expected DiagUnknownProperty in network block, got %v", res.Diagnostics)
	}
	nb := res.File.Agents[0].Sandbox.Network
	if nb == nil {
		t.Fatal("expected network block to survive recovery")
	}
	// mode + rules must be captured around the error.
	assertEq(t, "Mode", nb.Mode, "allowlist")
	if len(nb.Rules) != 1 || nb.Rules[0] != "github.com" {
		t.Errorf("Rules lost after error: %v", nb.Rules)
	}
}
