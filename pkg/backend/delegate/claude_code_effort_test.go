package delegate

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
)

// Anthropic's effort dial starts at low: no Claude model carries "none",
// so the delegate clamps it rather than handing the CLI a level it would
// reject — the same floor the claw route applies through coerceEffort.
func TestClaudeCodeEffort_NoneClampsToLow(t *testing.T) {
	for in, want := range map[string]string{
		"none":      "low",
		"low":       "low",
		"medium":    "medium",
		"high":      "high",
		"xhigh":     "xhigh",
		"max":       "max",
		"ultracode": "ultracode", // already wire-remapped upstream; defensive passthrough
	} {
		if got := claudeCodeEffort(in); got != want {
			t.Errorf("claudeCodeEffort(%q) = %q, want %q", in, got, want)
		}
	}
}

// The coercion has to hold where the env var is actually set — both claude
// spawns read CLAUDE_CODE_EFFORT_LEVEL through perTaskSpawnOpts, so an
// operator's explicit `reasoning_effort: none` must never reach the CLI
// verbatim on a route that cannot serve it.
func TestPerTaskSpawnOpts_CoercesNoneEffort(t *testing.T) {
	env, _ := claudesdk.ResolveSpawn(perTaskSpawnOpts(Task{ReasoningEffort: "none"})...)
	if env["CLAUDE_CODE_EFFORT_LEVEL"] != "low" {
		t.Errorf("CLAUDE_CODE_EFFORT_LEVEL = %q, want %q (none clamps to the dial's floor)", env["CLAUDE_CODE_EFFORT_LEVEL"], "low")
	}

	keepEnv, _ := claudesdk.ResolveSpawn(perTaskSpawnOpts(Task{ReasoningEffort: "high"})...)
	if keepEnv["CLAUDE_CODE_EFFORT_LEVEL"] != "high" {
		t.Errorf("CLAUDE_CODE_EFFORT_LEVEL = %q, want %q (supported levels pass through)", keepEnv["CLAUDE_CODE_EFFORT_LEVEL"], "high")
	}
}
