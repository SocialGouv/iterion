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

// A `tool` node is a deterministic shell action the gate does not govern
// (C112) — except through rung 4 of its Verified Action, where agentRecovery
// synthesises an agent and dispatches it with real tools. That agent carries
// no declarations of its own, so the WORKFLOW block is the only gate over
// it, and the tool node's own `permission:` never reaches it. Both halves
// are authoring facts the inspector's workflow-permission help states, and
// neither has another witness: C112 fires only on the tool node's own
// `permission:`, and C111 stays silent on a tool-only workflow precisely
// because the rung reads the workflow's lists (pkg/dsl/ir,
// TestPermissionRulesReachNoGatedNodeMoreShapes).

// recoveryRungBackend records the Task its Execute received: the task is
// where the resolved policy crosses to a backend, in-process or not. The
// call count is the other fact a rung witness needs — whether the goal was
// handed to an agent at all.
type recoveryRungBackend struct {
	task  delegate.Task
	calls int
}

func (b *recoveryRungBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.task = task
	b.calls++
	return delegate.Result{Output: map[string]any{"ok": true}}, nil
}

// recoveryRungPolicy drives the REAL rung-4 entry point — agentRecovery
// builds the synthetic node and goes through executeBackend, so a policy
// resolved from the wrong declarations anywhere on that path reddens, not
// only one resolved from a hand-built node shape. The answer is nil when
// the gate is off: buildTask leaves Task.Permission unset rather than
// shipping a disabled policy.
func recoveryRungPolicy(t *testing.T, wf *ir.Workflow, node *ir.ToolNode) *permission.Policy {
	t.Helper()
	// The mode chain ends at ITERION_PERMISSION, read from the host at
	// executor construction: a machine that exports it would otherwise arm
	// the gate under the test's feet.
	t.Setenv("ITERION_PERMISSION", "")
	rec := &recoveryRungBackend{}
	reg := delegate.NewRegistry()
	reg.Register("test_backend", rec)
	wf.DefaultBackend = "test_backend"
	e := NewClawExecutor(NewRegistry(), wf,
		WithLogger(iterlog.Nop()), WithWorkDir(t.TempDir()), WithBackendRegistry(reg))
	if err := e.agentRecovery(context.Background(), node, map[string]any{}, "postcondition"); err != nil {
		t.Fatalf("agentRecovery: %v", err)
	}
	return rec.task.Permission
}

func recoveryRungToolNode() *ir.ToolNode {
	return &ir.ToolNode{
		BaseNode:      ir.BaseNode{ID: "ship"},
		Command:       "rm -rf ./dist",
		Goal:          "the release artefacts are rebuilt",
		Postcondition: "./check.sh",
		Policy:        "recover",
		Recovery: &ir.RecoverySpec{
			MaxAgentAttempts: 1,
			Model:            "anthropic/claude-opus-5",
			AgentTools:       []string{"bash"},
		},
	}
}

// TestWorkflowGateGovernsAToolNodesRecoveryAgent: the workflow block alone
// arms and bounds the rung-4 agent. Red when agentRecovery stops going
// through the executor's task build, when the workflow's lists stop reaching
// a node that declares none, or when the rung agent acquires declarations of
// its own (which would REPLACE them and widen it).
//
// The fixture is built so each of the three lists is load-bearing for a
// DIFFERENT verdict, the way nodeRulesWorkflow is — under mode deny an
// UNMATCHED call is denied by the MODE, so a deny rule alone would hold
// whether or not it ever reached the rung:
//
//	permission: deny   allow: ["Bash(git:*)"]   deny: ["Bash(git push:*)"]
//
// `git diff` is allowed only because the ALLOW reached the rung; `git push`
// is denied only because the DENY did (that allow would otherwise permit
// it); `rm -rf /` is denied only because the allow is NARROW.
func TestWorkflowGateGovernsAToolNodesRecoveryAgent(t *testing.T) {
	pol := recoveryRungPolicy(t, &ir.Workflow{
		Permission:      "deny",
		PermissionAllow: []string{"Bash(git:*)"},
		PermissionDeny:  []string{"Bash(git push:*)"},
	}, recoveryRungToolNode())

	if pol == nil || !pol.Enabled() {
		t.Fatal("the workflow declares permission: deny — the rung's gate must be armed")
	}
	assertDecision(t, pol, "Bash", map[string]any{"command": "git diff HEAD"},
		permission.Allow, "the workflow's allow: reaches the rung-4 agent")
	assertDecision(t, pol, "Bash", map[string]any{"command": "git push origin main"},
		permission.Deny, "the workflow's deny: outranks its own allow on the rung-4 agent")
	assertDecision(t, pol, "Bash", map[string]any{"command": "rm -rf /"},
		permission.Deny, "the workflow's allow: bounds the rung-4 agent, it does not open Bash")
	// The same lists cross VERBATIM to a backend that does not share the
	// process (a sandboxed runner's permission_policy envelope, a hook's
	// base64 argv), so the serialised form is asserted too: the rung agent
	// declares none of its own, and a list widened on the way out would gate
	// the host run and not the container's.
	cfg := pol.Config()
	if !slices.Equal(cfg.Allow, []string{"Bash(git:*)"}) ||
		!slices.Equal(cfg.Deny, []string{"Bash(git push:*)"}) || len(cfg.Ask) != 0 {
		t.Errorf("the rung agent ships allow %v / ask %v / deny %v; want exactly the workflow's lists — it declares none of its own",
			cfg.Allow, cfg.Ask, cfg.Deny)
	}
}

// TestOnlyPolicyRecoverHandsTheGoalToTheRungAgent pins the CONDITION under
// which the two tests above matter, and which C112's remedy and
// docs/permissions.md state to the author: the rung needs `policy: recover`
// AND `recovery.max_agent_attempts > 0`. The bound alone stops the ladder at
// the recipe, so no agent is dispatched and the workflow's gate governs
// nothing on such a node — a remedy naming half the condition sends the
// author to gate a rung that never runs.
//
// This drives the whole ladder (executeToolNode → executeVerifiedToolNode),
// not agentRecovery directly, because the policy gate being measured is the
// ladder's, and counts the calls the backend received: an unmet
// postcondition under a non-recover policy must dispatch nobody.
func TestOnlyPolicyRecoverHandsTheGoalToTheRungAgent(t *testing.T) {
	for _, tc := range []struct {
		policy string
		want   int
	}{
		{"", 0},
		{ir.PolicyRequired, 0},
		{ir.PolicyBestEffort, 0},
		{ir.PolicyRecover, 1},
	} {
		name := tc.policy
		if name == "" {
			name = "unset"
		}
		t.Run("policy_"+name, func(t *testing.T) {
			t.Setenv("ITERION_PERMISSION", "")
			rec := &recoveryRungBackend{}
			reg := delegate.NewRegistry()
			reg.Register("test_backend", rec)
			e := NewClawExecutor(NewRegistry(), &ir.Workflow{DefaultBackend: "test_backend"},
				WithLogger(iterlog.Nop()), WithWorkDir(t.TempDir()), WithBackendRegistry(reg))
			node := recoveryRungToolNode()
			node.Command = "true"
			node.Postcondition = "false" // never met: the ladder escalates as far as it is allowed to
			node.Policy = tc.policy

			// The run's own error is the ladder's verdict on an unmet
			// postcondition (required/recover fail, best_effort continues);
			// what is under test is how far it escalated.
			_, err := e.executeToolNode(context.Background(), node, map[string]any{})
			if rec.calls != tc.want {
				t.Errorf("policy %q with max_agent_attempts: 1 dispatched %d agent call(s), want %d (run err: %v)",
					tc.policy, rec.calls, tc.want, err)
			}
		})
	}
}

// TestAToolNodesOwnPermissionNeverReachesItsRecoveryAgent is the other half
// of C112, and the reason the workflow block is the rung's ONLY gate: the
// synthetic agent is built from the recovery spec, not from the tool node's
// declarations — and a tool node has no rule lists to declare in the first
// place (ir.ToolNode carries `Permission` alone). An author who gates the
// tool node instead of the workflow ships nothing, which is what C112 warns
// about at compile time.
func TestAToolNodesOwnPermissionNeverReachesItsRecoveryAgent(t *testing.T) {
	node := recoveryRungToolNode()
	node.Permission = "deny"

	if pol := recoveryRungPolicy(t, &ir.Workflow{}, node); pol != nil {
		t.Fatalf("a tool node's own permission: is not enforced (C112) — it must not arm its recovery agent either; got %+v", pol.Config())
	}
}
