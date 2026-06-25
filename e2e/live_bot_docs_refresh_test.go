//go:build live

package e2e

import (
	"os"
	"testing"
	"time"
)

// TestLive_Bot_DocsRefresh runs the docs-refresh bot (Doki) against a
// fixture with a deliberate doc/code drift: the README documents a wrong
// function signature. Doki is a cross-family alternating review loop that
// detects drift, fixes the docs (never code logic), and converges.
//
// Reliability invariants: the scan + manifest + at least one reviewer
// fire; the run converges (streak_check.stop) or exhausts its bounded
// loop (both acceptable); and the drift gets corrected (a doc commit
// lands, or streak_check reports zero remaining drift). The quality panel
// then grades the doc edits + value.
//
// Requires: claude CLI (reviewer_claude/fix_claude) + OpenAI (reviewer_gpt).
// Expected: ~20-40 min.
func TestLive_Bot_DocsRefresh(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")
	requireOpenAI(t)

	workspaceDir, err := os.MkdirTemp("", "iterion-docs-refresh-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Logf("Workspace (persists): %s", workspaceDir)

	// Drift: README documents Add(x, y, z int) — three args — but the code
	// defines Add(a, b int). A doc-alignment pass must correct the README.
	files := map[string]string{
		"go.mod": "module iterion-live-fixture\n\ngo 1.25\n",
		"math.go": `package fixture

// Add returns the sum of a and b.
func Add(a, b int) int { return a + b }
`,
		"README.md": `# fixture

## API

` + "`Add(x, y, z int) int`" + ` returns the sum of its three arguments.
Call it with exactly three integers.
`,
	}
	seedCommits := seedGoModuleFixture(t, workspaceDir, files)

	vars := map[string]interface{}{
		"workspace_dir":         workspaceDir,
		"scope_notes":           "Align README with the actual Go API.",
		"max_review_iterations": 6, // bound cost; the fixture is tiny
		"diff_since":            "",
	}
	res := runBotLive(t, liveSpec{
		runIDBase:    "live-docs-refresh",
		botFile:      "docs-refresh/main.bot",
		workspaceDir: workspaceDir,
		vars:         vars,
		inputs:       vars,
		timeout:      40 * time.Minute,
	})

	// Reliability invariants: the scan + manifest + a reviewer must fire,
	// and the convergence gate must have been evaluated at least once.
	assertNodesFinished(t, res.events, "scan_docs", "build_manifest")
	if countFinished(res.events, "reviewer_claude")+countFinished(res.events, "reviewer_gpt") == 0 {
		t.Errorf("expected at least one reviewer to fire")
	}
	if countFinished(res.events, "streak_check") == 0 {
		t.Errorf("expected the convergence gate (streak_check) to be evaluated")
	}

	// The drift should be corrected: either a doc commit landed, or the
	// final manifest reports zero remaining drift (already-aligned).
	committed := workspaceCommitCount(t, workspaceDir) > seedCommits
	manifest, _ := lastNodeOutput(res.events, "build_manifest")
	zeroDrift := manifest != nil && asFloat(manifest["drifted_anchors"]) == 0
	if !committed && !zeroDrift {
		t.Errorf("expected a doc fix commit OR zero residual drift; got neither (commits=%d seed=%d drifted=%v)",
			workspaceCommitCount(t, workspaceDir), seedCommits, manifest["drifted_anchors"])
	} else {
		t.Logf("docs-refresh outcome: committed=%v zeroResidualDrift=%v", committed, zeroDrift)
	}

	assessQuality(t, res, qualityInput{
		kind:          "bot",
		name:          "docs-refresh",
		persona:       "Doki",
		primaryFamily: "anthropic",
		task:          "Correct a README that documents a wrong function signature (Add(x,y,z) vs Add(a,b)); align docs to code, no code-logic edits.",
		workProduct:   gitArtifactEvidence(t, workspaceDir),
	})
}
