//go:build live

package e2e

import (
	"os"
	"testing"
	"time"
)

// TestLive_Bot_DocsRefresh runs the docs-refresh bot (Doki) against a
// fixture with a deliberate doc/code drift: the README documents a wrong
// function signature. v3 (adaptive): a deterministic scan_hints node
// hands ONE campaign agent an ADVISORY report; the campaign explores the
// repo, fixes the docs (never code logic) committing in stride, then the
// deterministic TRUTH gates (scope, build) re-check.
//
// Reliability invariants: scan + campaign + gate fire; and the drift
// gets corrected (a doc commit lands, or the campaign honestly reports
// the docs aligned). The quality panel then grades the doc edits +
// value.
//
// Requires: claude CLI. Expected: ~10-30 min.
func TestLive_Bot_DocsRefresh(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")

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

	vars := map[string]any{
		"workspace_dir": workspaceDir,
		"scope_notes":   "Align README with the actual Go API.",
		"max_passes":    3, // bound cost; the fixture is tiny
		"diff_since":    "",
	}
	res := runBotLive(t, liveSpec{
		runIDBase:    "live-docs-refresh",
		botFile:      "docs-refresh/main.bot",
		workspaceDir: workspaceDir,
		vars:         vars,
		inputs:       vars,
		timeout:      40 * time.Minute,
	})

	// Reliability invariants: the advisory scan + the campaign must fire,
	// and the convergence gate must have been evaluated.
	assertNodesFinished(t, res.events, "scan_hints", "campaign", "scope_check", "verify_run", "gate")

	// The drift should be corrected: either a doc commit landed, or the
	// campaign honestly reports the docs aligned (already-aligned repo).
	committed := workspaceCommitCount(t, workspaceDir) > seedCommits
	camp, _ := lastNodeOutput(res.events, "campaign")
	aligned := camp != nil && camp["docs_aligned"] == true
	if !committed && !aligned {
		t.Errorf("expected a doc fix commit OR an honest docs_aligned report; got neither (commits=%d seed=%d docs_aligned=%v)",
			workspaceCommitCount(t, workspaceDir), seedCommits, camp["docs_aligned"])
	} else {
		t.Logf("docs-refresh outcome: committed=%v docsAligned=%v", committed, aligned)
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
