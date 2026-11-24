package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/sandbox/netproxy"
)

// Complementary coverage for sandbox.go pure helpers not already
// exercised by sandbox_internal_test.go. Focus: containsClawNode,
// backendIsClaw, cloneStringMap, fromIRSpec, ResolveNetworkPolicy,
// EngineRepoRoot. End-to-end startSandbox / shutdown remain
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
		// An explicit `auto` names no route: where such a node runs is the
		// chain's answer, and the union reads it from the resolver arm
		// (resolverRoutesToClaw), never from this field — see
		// TestContainsClawNode_ReadsAnAutoNodeOnTheRouteDispatchResolves.
		{"auto", false},
		{"AUTO", false},
		{"claude_code", false},
		{"codex", false},
		{"openai", false},
		// Env-templated backends must be expanded before the check — the
		// sec-audit-source/-deps `${ITERION_SEC_AUDIT_BACKEND:-claw}` pattern
		// reaches us verbatim in the IR. A definitely-unset var exercises the
		// `:-default` branch deterministically (regression: the unexpanded
		// template used to read as non-claw, skipping the iterion bind-mount).
		{"${ITERION_TEST_UNSET_BACKEND_XYZ:-claw}", true},
		{"${ITERION_TEST_UNSET_BACKEND_XYZ:-claude_code}", false},
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
			if got := containsClawNode(c.wf, nil); got != c.want {
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
	mode, rules, err := ResolveNetworkPolicy(nil)
	if err != nil {
		t.Fatalf("ResolveNetworkPolicy(nil): %v", err)
	}
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
	mode, rules, err := ResolveNetworkPolicy(spec)
	if err != nil {
		t.Fatalf("ResolveNetworkPolicy: %v", err)
	}
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
	mode, _, err := ResolveNetworkPolicy(spec)
	if err != nil {
		t.Fatalf("ResolveNetworkPolicy: %v", err)
	}
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
	mode, rules, err := ResolveNetworkPolicy(spec)
	if err != nil {
		t.Fatalf("ResolveNetworkPolicy: %v", err)
	}
	if mode != netproxy.ModeAllowlist {
		t.Errorf("mode = %q, want allowlist", mode)
	}
	if len(rules) != 1 || rules[0] != "only-this.example.com" {
		t.Errorf("rules with unknown preset should contain only user rule, got %v", rules)
	}
}

func TestEngineRepoRoot_NonEmptyWorkDirPassesThrough(t *testing.T) {
	// EngineRepoRoot returns workDir verbatim when non-empty (and not
	// inside a worktree subdirectory — that fallback is exercised when
	// .git is a file pointing into a worktrees subdir, which we don't
	// stage here).
	tmp := t.TempDir()
	got := EngineRepoRoot(tmp)
	if got != tmp {
		t.Errorf("got %q, want %q", got, tmp)
	}
}

func TestEngineRepoRoot_EmptyWorkDirFallsBackToCwd(t *testing.T) {
	got := EngineRepoRoot("")
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
	// A real worktree layout — git(1) builds it, because the pointer walk
	// behind EngineRepoRoot sanity-checks the origin with `git rev-parse`,
	// which a hand-written .git skeleton does not survive.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on PATH")
	}
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	worktree := filepath.Join(tmp, "feat-x")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.InitRepo(t, origin)
	gittest.Run(t, origin, "worktree", "add", "--quiet", worktree)

	got := EngineRepoRoot(worktree)
	// The engine's own worktree path resolves to the origin repo
	// (wtCtx.repoRoot is gitlib.FindMainRepoRoot); this helper must agree,
	// because the runner's secret gate and runview's fallback screen call
	// it to predict what the engine will do.
	if got != origin {
		t.Errorf("got %q, want origin repo %q", got, origin)
	}
}

// The rules' env expansion (#2099): a static (catalog) bot cannot name the
// deployment's gateway host — it is environment, unknowable to the author —
// so network rules resolve ${VAR} / ${VAR:-default} against the process env
// at policy resolution, exactly like model:/backend:/command:/timeout:.
// Resolved HERE (runner-side, where the proxy starts), not at DSL compile:
// in cloud the compile happens on the server and the sandbox runs on the
// runner pod whose env holds the gateway's name.
func TestResolveNetworkPolicy_ExpandsEnvInRules(t *testing.T) {
	t.Setenv("ITERION_TEST_GW_HOST", "gateway.corp.internal")
	spec := &sandbox.Spec{
		Network: &sandbox.Network{
			Mode:  sandbox.NetworkModeAllowlist,
			Rules: []string{"${ITERION_TEST_GW_HOST}", "${ITERION_TEST_GW_ALT:-fallback.example.com}", "**.anthropic.com"},
		},
	}
	_, rules, err := ResolveNetworkPolicy(spec)
	if err != nil {
		t.Fatalf("ResolveNetworkPolicy: %v", err)
	}
	want := []string{"gateway.corp.internal", "fallback.example.com", "**.anthropic.com"}
	if len(rules) != len(want) {
		t.Fatalf("rules = %v, want %v", rules, want)
	}
	for i := range want {
		if rules[i] != want[i] {
			t.Errorf("rules[%d] = %q, want %q", i, rules[i], want[i])
		}
	}
	// The compiled policy answers for the expanded host, not the reference.
	p, err := netproxy.Compile(netproxy.ModeAllowlist, rules)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !p.Allow("gateway.corp.internal:443") {
		t.Error("the env-named gateway host is not allowed — the expansion never reached the policy")
	}
	if p.Allow("elsewhere.example.com:443") {
		t.Error("an unlisted host passed the allowlist")
	}
}

// A reference that resolves to nothing is a loud refusal, never a silently
// dropped rule: dropped in allowlist mode it fails closed (merely confusing),
// but dropped in denylist mode it fails OPEN — the host the author denied is
// allowed. The run refuses to start and names the rule instead.
func TestResolveNetworkPolicy_UnsetRuleVarRefuses(t *testing.T) {
	spec := &sandbox.Spec{
		Network: &sandbox.Network{
			Mode:  sandbox.NetworkModeDenylist,
			Rules: []string{"${ITERION_TEST_UNSET_NEVER_SET}", "ok.example.com"},
		},
	}
	_, _, err := ResolveNetworkPolicy(spec)
	if err == nil {
		t.Fatal("an unset rule variable must refuse, not silently drop the rule")
	}
	if !strings.Contains(err.Error(), "${ITERION_TEST_UNSET_NEVER_SET}") {
		t.Errorf("the refusal does not name the offending rule: %v", err)
	}
}
