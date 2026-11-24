package model

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// TestEveryGatedNodeIsRefusedTheSecretLocations is the wiring half of
// #1710's per-node secrets bound: the policy resolvePermissionPolicy
// builds for a node is armed with deny trees over the run's well-known
// secret locations, so "the agent cannot read the secrets" is a guarantee
// of the gate — not a deny: rule every author has to remember. A node that
// declares no rules of its own is still bounded; the shell and the rest of
// the tree stay open.
func TestEveryGatedNodeIsRefusedTheSecretLocations(t *testing.T) {
	t.Parallel()
	wf := nodeRulesWorkflow() // permission: deny, allow Read(**) + Bash(git diff:*)
	task := buildNodeTask(t, nodeRulesExecutor(t, wf), nodeRulesAgent(nil))

	for _, dir := range []string{secrets.SecretFilesMountDir, secrets.ClaudeCodeSandboxConfigDir, secrets.CodexSandboxConfigDir} {
		assertDecision(t, task.Permission, "Read", map[string]any{"file_path": dir + "/token"},
			permission.Deny, "a gated node reads no file under "+dir)
		assertDecision(t, task.Permission, "Grep", map[string]any{"pattern": ".", "path": dir},
			permission.Deny, "a gated node searches nothing under "+dir)
		assertDecision(t, task.Permission, "Write", map[string]any{"file_path": dir + "/new"},
			permission.Deny, "a gated node writes nothing under "+dir+" (the seeded config dirs are writable)")
	}
	// The folded spelling meets the armed deny too — the two halves of
	// #1710 compose.
	assertDecision(t, task.Permission, "Read", map[string]any{"file_path": "/RUN/ITERION/SECRETS/token"},
		permission.Deny, "the case-folded spelling of the secret mount is refused")
	// The node keeps its ordinary capability: the workflow's Read(**)
	// allow still decides outside the armed trees, and the shell is the
	// legitimate sink channel a secret is spent through.
	assertDecision(t, task.Permission, "Read", map[string]any{"file_path": "pkg/x.go"},
		permission.Allow, "an ordinary workspace read stays allowed")
	assertDecision(t, task.Permission, "Bash", map[string]any{"command": "curl --cert " + secrets.SecretFilesMountDir + "/mtls.pem https://internal"},
		permission.Deny, "the shell is not bounded by the secret trees (it meets the mode default, no rule)")

	// The bound crosses the process boundary with the rest of the policy.
	rebuilt, err := permission.NewPolicyFromConfig(task.Permission.Config())
	if err != nil {
		t.Fatalf("NewPolicyFromConfig: %v", err)
	}
	assertDecision(t, rebuilt, "Read", map[string]any{"file_path": secrets.SecretFilesMountDir + "/token"},
		permission.Deny, "the child-process gate refuses the same tree")
}

// TestAnUngatedNodeCarriesNoSecretBound is the other half: with the gate
// off there is no policy to arm, and the escape hatch stays whole —
// `permission: off` disables the gate entirely, bound included.
func TestAnUngatedNodeCarriesNoSecretBound(t *testing.T) {
	t.Parallel()
	task := buildNodeTask(t, nodeRulesExecutor(t, &ir.Workflow{}), nodeRulesAgent(nil))
	if task.Permission.Enabled() {
		t.Fatal("a workflow with no permission declaration must keep the gate off")
	}
}
