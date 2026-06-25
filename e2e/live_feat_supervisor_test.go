//go:build live

package e2e

import (
	"os"
	"testing"
	"time"
)

// TestLive_Feat_Supervisor exercises an inline `supervisor` declaration via
// examples/supervisor/sample.bot: a watchdog supervisor watches the
// `implement` agent node concurrently and may enqueue steering messages.
// The feature works end-to-end when the supervisor compiles + spawns, the
// supervised node runs, and the run completes without the coordinator
// breaking it (steering is best-effort and monitor-triggered, so we assert
// the supervised node finishes, not that an intervention fired).
//
// Requires: claude CLI (implement) + ANTHROPIC_API_KEY (watchdog runs claw
// opus). Expected: ~5-15 min.
func TestLive_Feat_Supervisor(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")
	requireEnv(t, "ANTHROPIC_API_KEY")

	workspaceDir, err := os.MkdirTemp("", "iterion-feat-supervisor-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	seedGoModuleFixture(t, workspaceDir, map[string]string{
		"calc.go": "package fixture\n\n// Add returns a + b.\nfunc Add(a, b int) int { return a + b }\n",
	})

	res := runBotLive(t, liveSpec{
		runIDBase:    "live-feat-supervisor",
		botFile:      "supervisor/sample.bot",
		workspaceDir: workspaceDir,
		timeout:      15 * time.Minute,
	})

	assertNodesFinished(t, res.events, "implement")

	assessQuality(t, res, qualityInput{
		kind:          "feature",
		name:          "supervisor",
		primaryFamily: "anthropic",
		task:          "Run an agent under a concurrent watchdog supervisor; the supervised node should complete with the coordinator armed.",
		workProduct:   gitArtifactEvidence(t, workspaceDir),
	})
}
