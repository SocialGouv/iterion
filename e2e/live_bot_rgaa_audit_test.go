//go:build live

package e2e

import (
	"os"
	"testing"
	"time"
)

// TestLive_Bot_RgaaAudit runs the rgaa-audit bot (Acci) against an HTML
// page seeded with blatant RGAA (accessibility) violations: an image with
// no alt, an unlabelled input, an empty link/button, and low-contrast
// text. Acci inventories the UI, audits theme-by-theme against the RGAA
// criteria (shipped as skills), and writes a dated conformance report.
//
// Board posting is disabled (post_to_board=false) so the run is
// side-effect-free beyond the report under audits/. Loaded as a bundle so
// the RGAA criteria skills mirror (its scan_health gate hard-fails
// without them).
//
// Reliability invariants (v2 ADR-058): inventory + the one campaign
// auditor fire, the campaign surfaces ≥1 non-conformity, and the report
// is written. Then
// the quality panel grades the findings + value.
//
// Requires: claude CLI + OpenAI. Expected: ~10-25 min.
func TestLive_Bot_RgaaAudit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")
	requireOpenAI(t)

	workspaceDir, err := os.MkdirTemp("", "iterion-rgaa-audit-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Logf("Workspace (persists): %s", workspaceDir)

	seedGitRepo(t, workspaceDir)
	writeWorkspaceFiles(t, workspaceDir, map[string]string{
		"index.html": `<!doctype html>
<html lang="fr">
<head><meta charset="utf-8"><title>Demo</title></head>
<body>
  <img src="logo.png">
  <form><input type="text"><button></button></form>
  <a href="/next"></a>
  <p style="color:#bbb;background:#cccccc">Texte à très faible contraste</p>
</body>
</html>
`,
	})
	gitCommitAll(t, workspaceDir, "chore: seed a11y fixture")

	vars := map[string]interface{}{
		"workspace_dir": workspaceDir,
		"post_to_board": false, // report-only; no board writes
		"scope_notes":   "Audit index.html for RGAA conformity.",
	}
	res := runBotLive(t, liveSpec{
		runIDBase:    "live-rgaa-audit",
		bundleDir:    "../bots/rgaa-audit",
		workspaceDir: workspaceDir,
		vars:         vars,
		inputs:       vars,
		timeout:      25 * time.Minute,
	})

	assertNodesFinished(t, res.events, "inventory", "campaign", "scan_health")
	assertOutputFieldsNonEmpty(t, res.events, "campaign", "candidates")
	if rc, ok := lastNodeOutput(res.events, "report_card"); ok {
		if p, _ := rc["report_path"].(string); p != "" {
			t.Logf("rgaa report written: %s", p)
		}
	}

	work := rgaaWorkProduct(res)
	assessQuality(t, res, qualityInput{
		kind:          "bot",
		name:          "rgaa-audit",
		persona:       "Acci",
		primaryFamily: "anthropic",
		task:          "Audit an HTML page with planted RGAA violations (missing alt/label, empty link/button, low contrast); surface non-conformities (board disabled).",
		workProduct:   work,
	})
}

func rgaaWorkProduct(res liveResult) string {
	rev, _ := lastNodeOutput(res.events, "campaign")
	rc, _ := lastNodeOutput(res.events, "report_card")
	work := "## campaign candidates\n"
	if rev != nil {
		work += sprintAny(rev["candidates"]) + "\nconformity_pct: " + sprintAny(rev["conformity_pct"])
	}
	if rc != nil {
		work += "\n\n## report summary\n" + sprintAny(rc["summary"])
	}
	return work
}
