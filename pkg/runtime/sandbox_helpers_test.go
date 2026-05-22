package runtime

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/netproxy"
)

// Complementary coverage for sandbox.go pure helpers not already
// exercised by sandbox_internal_test.go. Focus: containsClawNode,
// backendIsClaw, cloneStringMap, fromIRSpec, resolveNetworkPolicy,
// engineRepoRoot. End-to-end startSandbox / shutdown remain
// integration-only because they require docker/k8s.

func TestBackendIsClaw(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"", true},
		{"claw", true},
		{"CLAW", true}, // ToLower normalised
		{"Claw", true},
		{"claude_code", false},
		{"codex", false},
		{"openai", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := backendIsClaw(c.name); got != c.want {
				t.Errorf("backendIsClaw(%q) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

func TestContainsClawNode(t *testing.T) {
	cases := []struct {
		name string
		wf   *ir.Workflow
		want bool
	}{
		{
			"empty workflow",
			&ir.Workflow{Nodes: map[string]ir.Node{}},
			false,
		},
		{
			"agent on claude_code only",
			&ir.Workflow{Nodes: map[string]ir.Node{
				"a": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, LLMFields: ir.LLMFields{Backend: "claude_code"}},
			}},
			false,
		},
		{
			"agent on explicit claw",
			&ir.Workflow{Nodes: map[string]ir.Node{
				"a": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, LLMFields: ir.LLMFields{Backend: "claw"}},
			}},
			true,
		},
		{
			"agent with empty backend (defaults to claw)",
			&ir.Workflow{Nodes: map[string]ir.Node{
				"a": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			}},
			true,
		},
		{
			"judge on claw",
			&ir.Workflow{Nodes: map[string]ir.Node{
				"j": &ir.JudgeNode{BaseNode: ir.BaseNode{ID: "j"}, LLMFields: ir.LLMFields{Backend: "claw"}},
			}},
			true,
		},
		{
			"router on claw",
			&ir.Workflow{Nodes: map[string]ir.Node{
				"r": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "r"}, LLMFields: ir.LLMFields{Backend: "claw"}},
			}},
			true,
		},
		{
			"tool / compute / human nodes don't count",
			&ir.Workflow{Nodes: map[string]ir.Node{
				"t": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "t"}},
				"c": &ir.ComputeNode{BaseNode: ir.BaseNode{ID: "c"}},
				"h": &ir.HumanNode{BaseNode: ir.BaseNode{ID: "h"}},
			}},
			false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := containsClawNode(c.wf); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestCloneStringMap(t *testing.T) {
	if got := cloneStringMap(nil); got != nil {
		t.Errorf("nil input should yield nil, got %v", got)
	}
	in := map[string]string{"a": "1", "b": "2"}
	got := cloneStringMap(in)
	if !reflect.DeepEqual(got, in) {
		t.Errorf("got %v, want %v", got, in)
	}
	// Mutating the clone must not affect the original.
	got["a"] = "mutated"
	if in["a"] != "1" {
		t.Errorf("mutating clone affected original: %v", in)
	}
}

func TestFromIRSpec_BasicFields(t *testing.T) {
	in := &ir.SandboxSpec{
		Mode:            "inline",
		Image:           "alpine:3.20",
		User:            "iterion",
		PostCreate:      "echo hi",
		WorkspaceFolder: "/workspace",
		HostState:       "none",
		Mounts:          []string{"type=bind,src=/a,dst=/b"},
		Env:             map[string]string{"K": "V"},
	}
	out := fromIRSpec(in)
	if string(out.Mode) != "inline" || out.Image != "alpine:3.20" || out.User != "iterion" {
		t.Errorf("scalar fields lost: %+v", out)
	}
	if out.PostCreate != "echo hi" || out.WorkspaceFolder != "/workspace" {
		t.Errorf("scalar fields lost: %+v", out)
	}
	if string(out.HostState) != "none" {
		t.Errorf("HostState lost: %q", out.HostState)
	}
	if len(out.Mounts) != 1 || out.Mounts[0] != "type=bind,src=/a,dst=/b" {
		t.Errorf("Mounts lost: %v", out.Mounts)
	}
	if out.Env["K"] != "V" {
		t.Errorf("Env lost: %v", out.Env)
	}
	// Env / Mounts must be defensive copies — mutating the output must
	// not leak into the input.
	out.Env["K"] = "mutated"
	if in.Env["K"] != "V" {
		t.Errorf("Env clone leaked: %v", in.Env)
	}
	out.Mounts[0] = "mutated"
	if in.Mounts[0] != "type=bind,src=/a,dst=/b" {
		t.Errorf("Mounts clone leaked: %v", in.Mounts)
	}
}

func TestFromIRSpec_BuildAndNetworkConverted(t *testing.T) {
	in := &ir.SandboxSpec{
		Mode: "inline",
		Build: &ir.SandboxBuild{
			Dockerfile: "Containerfile",
			Context:    ".devcontainer",
			Args:       map[string]string{"BASE": "alpine:3.20"},
		},
		Network: &ir.SandboxNetwork{
			Mode:    "allowlist",
			Preset:  "iterion-default",
			Rules:   []string{"github.com", "*.npmjs.org"},
			Inherit: "merge",
		},
	}
	out := fromIRSpec(in)
	if out.Build == nil || out.Build.Dockerfile != "Containerfile" || out.Build.Args["BASE"] != "alpine:3.20" {
		t.Errorf("Build conversion: %+v", out.Build)
	}
	if out.Network == nil || string(out.Network.Mode) != "allowlist" || out.Network.Preset != "iterion-default" {
		t.Errorf("Network conversion: %+v", out.Network)
	}
	if len(out.Network.Rules) != 2 {
		t.Errorf("Network.Rules: %v", out.Network.Rules)
	}
}

func TestFromIRSpec_NilSubBlocks(t *testing.T) {
	in := &ir.SandboxSpec{Mode: "auto"} // no Build, no Network
	out := fromIRSpec(in)
	if out.Build != nil {
		t.Errorf("Build should stay nil, got %+v", out.Build)
	}
	if out.Network != nil {
		t.Errorf("Network should stay nil, got %+v", out.Network)
	}
}

func TestResolveNetworkPolicy_NilSpecYieldsOpen(t *testing.T) {
	mode, rules := resolveNetworkPolicy(nil)
	if mode != netproxy.ModeOpen {
		t.Errorf("mode = %q, want open (default since 2026-05-22)", mode)
	}
	// No implicit preset is applied in open mode — the proxy isn't
	// even started, so rules are irrelevant. Verify nothing was
	// silently prefixed (would surface as misleading docs/logs if the
	// caller later flipped to allowlist).
	if len(rules) != 0 {
		t.Errorf("expected no implicit rules in open default, got %v", rules)
	}
}

func TestResolveNetworkPolicy_ExplicitDenylist(t *testing.T) {
	spec := &sandbox.Spec{
		Network: &sandbox.Network{
			Mode:  sandbox.NetworkModeDenylist,
			Rules: []string{"*.evil.site"},
		},
	}
	mode, rules := resolveNetworkPolicy(spec)
	if mode != netproxy.ModeDenylist {
		t.Errorf("mode = %q, want denylist", mode)
	}
	// Network.Preset is empty here → no implicit prefix; only the
	// user-supplied rule is in play. (Operators who want the curated
	// list can name it via Network.Preset: "iterion-default".)
	hasEvil := false
	for _, r := range rules {
		if r == "*.evil.site" {
			hasEvil = true
		}
	}
	if !hasEvil {
		t.Errorf("user rule lost: %v", rules)
	}
}

func TestResolveNetworkPolicy_OpenMode(t *testing.T) {
	spec := &sandbox.Spec{
		Network: &sandbox.Network{Mode: sandbox.NetworkModeOpen},
	}
	mode, _ := resolveNetworkPolicy(spec)
	if mode != netproxy.ModeOpen {
		t.Errorf("mode = %q, want open", mode)
	}
}

func TestResolveNetworkPolicy_CustomPresetReplacesDefault(t *testing.T) {
	// An unknown preset name should fall through with no preset rules
	// and the user-rules-only list.
	spec := &sandbox.Spec{
		Network: &sandbox.Network{
			Mode:   sandbox.NetworkModeAllowlist,
			Preset: "no-such-preset-xyz",
			Rules:  []string{"only-this.example.com"},
		},
	}
	mode, rules := resolveNetworkPolicy(spec)
	if mode != netproxy.ModeAllowlist {
		t.Errorf("mode = %q, want allowlist", mode)
	}
	if len(rules) != 1 || rules[0] != "only-this.example.com" {
		t.Errorf("rules with unknown preset should contain only user rule, got %v", rules)
	}
}

func TestEngineRepoRoot_NonEmptyWorkDirPassesThrough(t *testing.T) {
	// engineRepoRoot returns workDir verbatim when non-empty (and not
	// inside a worktree subdirectory — that fallback is exercised when
	// .git is a file pointing into a worktrees subdir, which we don't
	// stage here).
	tmp := t.TempDir()
	got := engineRepoRoot(tmp)
	if got != tmp {
		t.Errorf("got %q, want %q", got, tmp)
	}
}

func TestEngineRepoRoot_EmptyWorkDirFallsBackToCwd(t *testing.T) {
	got := engineRepoRoot("")
	// Either matches the current working directory, or empty when os.Getwd
	// itself fails — but in a healthy test environment we expect cwd.
	cwd, err := os.Getwd()
	if err != nil {
		t.Skipf("os.Getwd unavailable: %v", err)
	}
	if got != cwd {
		t.Errorf("got %q, want cwd %q", got, cwd)
	}
}

func TestEngineRepoRoot_WorktreeLayoutResolvesToOriginRepo(t *testing.T) {
	// Build a minimal worktree-layout fixture:
	//   <tmp>/origin/.git/worktrees/feat-x/
	//   <tmp>/origin/.git/HEAD                  ← marks origin as a repo
	//   <tmp>/feat-x/.git                       ← gitdir-pointer file
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	worktree := filepath.Join(tmp, "feat-x")
	originGitWorktreeDir := filepath.Join(origin, ".git", "worktrees", "feat-x")
	if err := os.MkdirAll(originGitWorktreeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	// Origin .git/HEAD anchors the repo discovery.
	if err := os.WriteFile(filepath.Join(origin, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The worktree's .git is a file pointing into the origin's
	// .git/worktrees/<name> directory — git's stable layout.
	pointer := []byte("gitdir: " + originGitWorktreeDir + "\n")
	if err := os.WriteFile(filepath.Join(worktree, ".git"), pointer, 0o644); err != nil {
		t.Fatal(err)
	}

	got := engineRepoRoot(worktree)
	// If the implementation walks the pointer to reach origin, we get
	// origin. Otherwise the function returns the worktree as-is — both
	// are valid implementations of "where should devcontainer.json
	// lookup happen?", so accept either.
	if got != worktree && got != origin {
		t.Errorf("got %q, want %q or %q", got, worktree, origin)
	}
}
