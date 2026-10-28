//go:build live

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLive_Bot_RgaaAudit runs the rgaa-audit bot (Acci v3) on the planted
// fixture site the engine contract tests use (bots/testdata/rgaa-audit-site:
// an iframe without a title, an empty link, an unlabelled field, an empty
// button, plus a TSX component). The ultra11y engine finds those; the
// read-only agents adjudicate the criteria it left open, batch by batch;
// the second reader puts the draft report on trial; the run writes the
// French report, the coverage annex and the issue plan.
//
// Board posting is off (post_to_board=false) and the rendered layer is off
// (no browser on the host): the run is side-effect-free beyond its own
// scratch directory and the report_dir it is given.
//
// Reliability invariants: the engine nodes fire, the engine finds ≥ 3
// defects, the adjudication arithmetic holds (applied + still open = the
// worklist), the report exists and reads as an RGAA report. Then the
// quality panel grades the agents' verdicts.
//
// Requires: claude CLI + OpenAI, and node (the engine). Expected: ~15-40 min.
func TestLive_Bot_RgaaAudit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")
	requireCLI(t, "node")
	requireOpenAI(t)

	workspaceDir, err := os.MkdirTemp("", "iterion-rgaa-audit-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Logf("Workspace (persists): %s", workspaceDir)
	if err := os.CopyFS(workspaceDir, os.DirFS(filepath.Join("..", "bots", "testdata", "rgaa-audit-site"))); err != nil {
		t.Fatalf("copy fixture site: %v", err)
	}
	seedGitRepo(t, workspaceDir)
	gitCommitAll(t, workspaceDir, "chore: seed a11y fixture")
	// The deliverables land beside the workspace, never inside the audited
	// tree (a report under it would be audited by the next run).
	reportDir := workspaceDir + "-audits"

	vars := map[string]any{
		"workspace_dir": workspaceDir,
		"post_to_board": false, // report only; no board writes
		"rendered":      "off", // no browser on the host
		"report_dir":    reportDir,
		"scope_notes":   "Audit the two HTML pages and the TSX component for RGAA conformity.",
	}
	res := runBotLive(t, liveSpec{
		runIDBase:    "live-rgaa-audit",
		bundleDir:    "../bots/rgaa-audit",
		workspaceDir: workspaceDir,
		vars:         vars,
		inputs:       vars,
		timeout:      45 * time.Minute,
	})

	assertNodesFinished(t, res.events, "prepare", "static_audit", "worklist", "merge_verdicts", "fold_final", "draft_report", "final_report", "deliver")
	if sa, ok := lastNodeOutput(res.events, "static_audit"); ok {
		if n, _ := sa["findings"].(float64); n < 3 {
			t.Errorf("the engine reported %v findings on a site with four planted defects", sa["findings"])
		}
	}
	wl, _ := lastNodeOutput(res.events, "worklist")
	fold, _ := lastNodeOutput(res.events, "fold_final")
	if wl != nil && fold != nil {
		count, _ := wl["count"].(float64)
		applied, _ := fold["applied"].(float64)
		open, _ := fold["still_open"].(float64)
		if applied+open != count {
			t.Errorf("adjudication arithmetic: applied %v + still open %v ≠ worklist %v", applied, open, count)
		}
		if applied == 0 {
			t.Errorf("the agents landed no verdict at all on %v criteria", count)
		}
	}
	final, _ := lastNodeOutput(res.events, "final_report")
	if final != nil {
		p, _ := final["report_path"].(string)
		body, err := os.ReadFile(p)
		if err != nil || !strings.Contains(string(body), "RGAA") {
			t.Errorf("no readable RGAA report at %q: %v", p, err)
		}
		t.Logf("rgaa report: %s (conformance %v%%, provisional %v, degraded %v)", p, final["conformance_pct"], final["provisional"], final["degraded"])
	}

	assessQuality(t, res, qualityInput{
		kind:          "bot",
		name:          "rgaa-audit",
		persona:       "Acci",
		primaryFamily: "anthropic",
		task:          "Adjudicate the RGAA criteria a static pass cannot decide on a small site with planted defects, then put the draft report on trial; the engine found and gated the rest (board disabled, no browser).",
		workProduct:   rgaaWorkProduct(res),
	})
}

func rgaaWorkProduct(res liveResult) string {
	var b strings.Builder
	for _, node := range []string{"static_audit", "worklist", "merge_verdicts", "fold_final", "draft_report", "merge_refutations", "refute_apply", "final_report", "deliver"} {
		if out, ok := lastNodeOutput(res.events, node); ok {
			fmt.Fprintf(&b, "## %s\n%s\n\n", node, sprintAny(out))
		}
	}
	if final, ok := lastNodeOutput(res.events, "final_report"); ok {
		for _, key := range []string{"report_path", "technical_annex_path", "annex_path"} {
			if p, _ := final[key].(string); p != "" {
				if raw, err := os.ReadFile(p); err == nil {
					fmt.Fprintf(&b, "## %s\n", key)
					b.Write(raw)
				}
			}
		}
	}
	return b.String()
}
