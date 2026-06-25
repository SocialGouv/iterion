//go:build live

package e2e

import (
	"os"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// TestLive_Feat_Permission_Deny exercises the permission gate (deny mode) —
// the anti-prompt-injection boundary. The workflow denies all Bash; a claw
// agent instructed to run a shell command must have its bash tool-call
// refused by the gate and then report it could not run.
//
// Reliability invariant (lenient OR, since LLM phrasing varies): either a
// tool-error event mentions the refusal, OR the agent reports ran_bash=false.
//
// Requires: ANTHROPIC_API_KEY (claw). Expected: ~2-5 min.
func TestLive_Feat_Permission_Deny(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireEnv(t, "ANTHROPIC_API_KEY")

	workspaceDir, err := os.MkdirTemp("", "iterion-feat-permission-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	seedGitRepo(t, workspaceDir)

	res := runBotLive(t, liveSpec{
		runIDBase:    "live-feat-permission-deny",
		botFile:      "feat_permission_deny.bot",
		workspaceDir: workspaceDir,
		timeout:      6 * time.Minute,
	})

	assertNodesFinished(t, res.events, "runner")
	refused := eventDataMentions(res.events, "permission", store.EventToolError) ||
		eventDataMentions(res.events, "refus", store.EventToolError) ||
		eventDataMentions(res.events, "denied", store.EventToolError) ||
		eventDataMentions(res.events, "not allowed", store.EventToolError)
	out, _ := lastNodeOutput(res.events, "runner")
	ranBash, _ := out["ran_bash"].(bool)
	if !refused && ranBash {
		t.Errorf("permission deny gate did not block Bash: no refusal tool-error AND agent reports ran_bash=true")
	} else {
		t.Logf("permission deny worked: refusalEvent=%v ran_bash=%v", refused, ranBash)
	}
}
