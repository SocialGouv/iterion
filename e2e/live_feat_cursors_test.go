//go:build live

package e2e

import (
	"os"
	"testing"
	"time"
)

// TestLive_Feat_Cursors exercises the cursor prompt-engineering dials via
// examples/cursors/sample.bot: a reviewer agent with `cursors: {ambition:
// ambitious, depth: 0.7}` activated. The feature works end-to-end when the
// cursor declarations compile, activate (the runtime appends a
// ## Calibration section to the system prompt), and the node returns a
// schema-valid verdict — without C083-C086 diagnostics breaking the run.
//
// Requires: ANTHROPIC_API_KEY (the reviewer runs claw anthropic).
// Expected: ~3-6 min.
func TestLive_Feat_Cursors(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireEnv(t, "ANTHROPIC_API_KEY")
	// The sample ships unpinned so readers can run it on whatever they
	// have; this proof is ABOUT claw, so the preference is pinned here —
	// on a host where Claude Code is signed in, the default order
	// (claude_code first) would otherwise prove the wrong backend. The
	// default override is cleared with it: it sits ABOVE the preference
	// in the resolution order and would silently win.
	t.Setenv("ITERION_DEFAULT_BACKEND", "")
	t.Setenv("ITERION_BACKEND_PREFERENCE", "claw")

	workspaceDir, err := os.MkdirTemp("", "iterion-feat-cursors-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	seedGoModuleFixture(t, workspaceDir, map[string]string{
		"x.go": "package fixture\n\nfunc X() int { return 1 }\n",
	})

	res := runBotLive(t, liveSpec{
		runIDBase:    "live-feat-cursors",
		botFile:      "cursors/sample.bot",
		workspaceDir: workspaceDir,
		timeout:      6 * time.Minute,
	})

	assertNodesFinished(t, res.events, "reviewer")
	assertSchemaValid(t, res.wf, res.events, "reviewer")
	assertOutputFieldsNonEmpty(t, res.events, "reviewer", "rationale", "recommendation")

	v, _ := lastNodeOutput(res.events, "reviewer")
	assessQuality(t, res, qualityInput{
		kind:          "feature",
		name:          "cursors",
		primaryFamily: "anthropic",
		task:          "Run a reviewer agent with ambition/depth cursors activated; produce a calibrated verdict.",
		workProduct:   "## verdict (cursors active)\nrationale: " + sprintAny(v["rationale"]) + "\nrecommendation: " + sprintAny(v["recommendation"]),
	})
}
