package model

import (
	"context"
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// nodeRulesWorkflow is the fixture every case below shares. It is built so
// REPLACE and MERGE reach DIFFERENT verdicts — the point a fixture whose two
// lists agree would miss entirely:
//
//	workflow deny: ["Read(.env*)"]   node deny: ["Bash"]
//	workflow allow: ["Read(**)", "Bash(git diff:*)"]
//
// Under replacement the node's deny list is [Bash] alone, so reading `.env`
// falls through to the workflow's allow and is ALLOWED. Under a merge it
// would be denied. Meanwhile `git diff` is denied either way — by the node's
// bare Bash deny — which is what a "node list dropped" mutation reddens.
func nodeRulesWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Permission:      "deny",
		PermissionAllow: []string{"Read(**)", "Bash(git diff:*)"},
		PermissionDeny:  []string{"Read(.env*)"},
	}
}

func nodeRulesExecutor(t *testing.T, wf *ir.Workflow, opts ...ClawExecutorOption) *ClawExecutor {
	t.Helper()
	base := []ClawExecutorOption{WithLogger(iterlog.Nop()), WithWorkDir(t.TempDir())}
	return NewClawExecutor(NewRegistry(), wf, append(base, opts...)...)
}

// buildNodeTask runs the REAL task-building path — extractBackendFields, then
// buildTask, then resolvePermissionPolicy — so a node list dropped anywhere
// between the IR and the policy reddens, not just one dropped in the
// resolver.
func buildNodeTask(t *testing.T, e *ClawExecutor, n *ir.AgentNode) delegate.Task {
	t.Helper()
	f, err := extractBackendFields(n)
	if err != nil {
		t.Fatalf("extractBackendFields: %v", err)
	}
	task, err := e.buildTask(context.Background(), n, f, map[string]any{}, delegate.BackendClaw, &nodeBuildSession{})
	if err != nil {
		t.Fatalf("buildTask: %v", err)
	}
	return task
}

func nodeRulesAgent(deny []string) *ir.AgentNode {
	return &ir.AgentNode{
		BaseNode:       ir.BaseNode{ID: "converge"},
		LLMFields:      ir.LLMFields{Backend: "claw", Model: "anthropic/claude-opus-5"},
		AutoMemory:     "off",
		PermissionDeny: deny,
	}
}

func assertDecision(t *testing.T, pol *permission.Policy, tool string, input map[string]any, want permission.Decision, why string) {
	t.Helper()
	got, rule := pol.Evaluate(tool, input)
	if got != want {
		t.Errorf("%s: Evaluate(%s, %v) = %v (rule %q), want %v", why, tool, input, got, rule, want)
	}
}

// TestBuiltTaskPolicyTakesTheNodeListOverTheWorkflowList is the runtime half
// of the feature: the policy the backend receives is built from the node's
// list, and only per kind — the node declared no allow:, so the workflow's
// allow: is still in force.
func TestBuiltTaskPolicyTakesTheNodeListOverTheWorkflowList(t *testing.T) {
	e := nodeRulesExecutor(t, nodeRulesWorkflow())
	task := buildNodeTask(t, e, nodeRulesAgent([]string{"Bash"}))
	if !task.Permission.Enabled() {
		t.Fatal("the gate must be enabled: the workflow declares permission: deny")
	}
	// Replacement: the workflow's Read(.env*) deny is GONE, so the
	// workflow's own Read(**) allow decides. A merge reddens this line.
	assertDecision(t, task.Permission, "Read", map[string]any{"file_path": ".env"},
		permission.Allow, "the node's deny: REPLACES the workflow's")
	// The node's own rule is in force. Dropping the node list reddens this.
	assertDecision(t, task.Permission, "Bash", map[string]any{"command": "git diff"},
		permission.Deny, "the node's bare Bash deny outranks the workflow's scoped allow")
}

// TestBuiltTaskPolicyInheritsTheWorkflowListWhenTheNodeDeclaresNone is the
// other half: no node list means the workflow's applies unchanged. Mutating
// EffectivePermissionRules to always return the node list reddens it.
func TestBuiltTaskPolicyInheritsTheWorkflowListWhenTheNodeDeclaresNone(t *testing.T) {
	e := nodeRulesExecutor(t, nodeRulesWorkflow())
	task := buildNodeTask(t, e, nodeRulesAgent(nil))
	assertDecision(t, task.Permission, "Read", map[string]any{"file_path": ".env"},
		permission.Deny, "an undeclared node list inherits the workflow's deny")
	assertDecision(t, task.Permission, "Bash", map[string]any{"command": "git diff"},
		permission.Allow, "an undeclared node list inherits the workflow's allow")
}

// TestBuiltTaskPolicyConfigCarriesExactlyTheResolvedLists asserts the
// SERIALISED form, which is what actually crosses to a backend that does not
// share the process: the pre-task permission_policy envelope of a sandboxed
// claw runner, and the base64 argv of the grok/kimi hook. A policy that
// evaluated right in-process but shipped the workflow's rules would gate the
// host run and not the container's.
func TestBuiltTaskPolicyConfigCarriesExactlyTheResolvedLists(t *testing.T) {
	e := nodeRulesExecutor(t, nodeRulesWorkflow())
	cfg := buildNodeTask(t, e, nodeRulesAgent([]string{"Bash"})).Permission.Config()

	if !slices.Equal(cfg.Deny, []string{"Bash"}) {
		t.Errorf("serialised deny = %v, want exactly the node's [Bash]", cfg.Deny)
	}
	if slices.Contains(cfg.Deny, "Read(.env*)") {
		t.Error("serialised deny still carries the REPLACED workflow rule")
	}
	if !slices.Equal(cfg.Allow, []string{"Read(**)", "Bash(git diff:*)"}) {
		t.Errorf("serialised allow = %v, want the workflow's (the node declared none)", cfg.Allow)
	}
	if cfg.Mode != "deny" {
		t.Errorf("serialised mode = %q, want deny", cfg.Mode)
	}
}

// TestRunLevelRulesStayAdditiveOverANodeList pins the second step of the
// resolution, which is deliberately NOT a replacement: --permission-deny is
// the operator's live escape hatch over a .bot they may not own, and a node
// list must not be able to take it away.
func TestRunLevelRulesStayAdditiveOverANodeList(t *testing.T) {
	// All THREE kinds are asserted, and each with a verdict only its own kind
	// can produce: swapping the run-level allow and ask lists at the
	// resolution site left the whole suite green when only deny was pinned.
	e := nodeRulesExecutor(t, nodeRulesWorkflow(),
		WithPermissionRules([]string{"Glob"}, []string{"WebFetch"}, []string{"Read(secrets/**)"}))
	task := buildNodeTask(t, e, nodeRulesAgent([]string{"Bash"}))

	assertDecision(t, task.Permission, "Read", map[string]any{"file_path": "secrets/key"},
		permission.Deny, "the run-level DENY applies on top of the node's list")
	assertDecision(t, task.Permission, "WebFetch", map[string]any{"url": "https://example.test"},
		permission.Ask, "the run-level ASK pauses — an allow here would mean the two lists were swapped")
	assertDecision(t, task.Permission, "Glob", map[string]any{"pattern": "**/*.go"},
		permission.Allow, "the run-level ALLOW approves without pausing — an ask here would mean the two lists were swapped")
	assertDecision(t, task.Permission, "Bash", map[string]any{"command": "git diff"},
		permission.Deny, "the node's list is still in force under run-level rules")
	assertDecision(t, task.Permission, "Read", map[string]any{"file_path": ".env"},
		permission.Allow, "a run-level rule does not resurrect the replaced workflow rule")
}

// TestJudgeNodeRulesReachThePolicy keeps the two node kinds honest: the DSL
// registers the three lists on agent AND judge through one shared property
// set, but the IR, the JSON seam and extractBackendFields each carry two
// duplicated arms — so a judge wired at three of four sites reads as working
// until a judge is the node that needs the bound.
func TestJudgeNodeRulesReachThePolicy(t *testing.T) {
	e := nodeRulesExecutor(t, nodeRulesWorkflow())
	j := &ir.JudgeNode{
		BaseNode:       ir.BaseNode{ID: "verdict"},
		LLMFields:      ir.LLMFields{Backend: "claw", Model: "anthropic/claude-opus-5"},
		AutoMemory:     "off",
		PermissionDeny: []string{"Bash"},
	}
	f, err := extractBackendFields(j)
	if err != nil {
		t.Fatalf("extractBackendFields: %v", err)
	}
	task, err := e.buildTask(context.Background(), j, f, map[string]any{}, delegate.BackendClaw, &nodeBuildSession{})
	if err != nil {
		t.Fatalf("buildTask: %v", err)
	}
	assertDecision(t, task.Permission, "Bash", map[string]any{"command": "git diff"},
		permission.Deny, "a judge's own deny: reaches the policy")
	assertDecision(t, task.Permission, "Read", map[string]any{"file_path": ".env"},
		permission.Allow, "a judge's deny: REPLACES the workflow's")
}
