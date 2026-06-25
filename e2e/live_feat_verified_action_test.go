//go:build live

package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLive_Feat_VerifiedAction exercises the Verified Action adaptive
// recovery quad (ADR-044): the tool's recipe (`true`) does not satisfy the
// postcondition (done.txt present), so the node must escalate down the
// recovery ladder to the agent rung, which creates done.txt to satisfy the
// deterministic postcondition — the postcondition being the truth oracle at
// every rung.
//
// Requires: ANTHROPIC_API_KEY (the recovery agent rung). Expected: ~3-8 min.
func TestLive_Feat_VerifiedAction(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireEnv(t, "ANTHROPIC_API_KEY")

	workspaceDir, err := os.MkdirTemp("", "iterion-feat-verified-action-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	seedGitRepo(t, workspaceDir)

	res := runBotLive(t, liveSpec{
		runIDBase:    "live-feat-verified-action",
		botFile:      "feat_verified_action.bot",
		workspaceDir: workspaceDir,
		timeout:      8 * time.Minute,
		withWorkDir:  true, // ${PROJECT_DIR} → the seeded workspace
	})

	assertNodesFinished(t, res.events, "create_done")
	if _, err := os.Stat(filepath.Join(workspaceDir, "done.txt")); err != nil {
		t.Errorf("expected the recovery ladder to create done.txt (postcondition satisfied), but it is absent: %v", err)
	} else {
		t.Logf("verified-action recovery satisfied the postcondition (done.txt present)")
	}

	assessQuality(t, res, qualityInput{
		kind:          "feature",
		name:          "verified-action",
		primaryFamily: "anthropic",
		task:          "A brittle ACTION tool node self-heals via the recovery ladder to satisfy a deterministic postcondition (create done.txt).",
		workProduct:   gitArtifactEvidence(t, workspaceDir),
	})
}
