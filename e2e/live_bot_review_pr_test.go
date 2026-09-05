//go:build live

package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLive_Bot_ReviewPR runs the review-pr bot (Revi) against a seeded
// base..branch diff that contains a deliberate, catchable bug (an
// unbounded slice index on request-controlled input). Revi is read-only:
// it fans out to a Claude + GPT reviewer pair, merges findings, and emits
// a markdown report (board writes disabled here via post_to_board=false).
//
// Reliability invariants: both reviewers fire, emit produces a
// schema-valid output, and at least one finding is surfaced for the
// planted bug. Then the quality panel grades the findings + value.
//
// Requires: claude CLI (reviewer_claude/emit) + OpenAI (reviewer_gpt).
// Expected: ~10-20 min, a few dollars.
func TestLive_Bot_ReviewPR(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")
	requireOpenAI(t)

	workspaceDir, err := os.MkdirTemp("", "iterion-review-pr-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Logf("Workspace (persists): %s", workspaceDir)

	// Base = a clean helper; branch = a handler with a planted out-of-bounds
	// read on a request-controlled index. The misleading comment is bait:
	// a competent reviewer must reject "always valid" and flag the missing
	// bounds check (panic / DoS).
	base := map[string]string{
		"go.mod": "module iterion-live-fixture\n\ngo 1.25\n",
		"util.go": `package fixture

// Clamp returns v bounded to [lo, hi].
func Clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
`,
	}
	branch := map[string]string{
		"handler.go": `package fixture

// Pick returns the item at the caller-supplied index.
// idx is always valid because the frontend validates it.
func Pick(items []string, idx int) string {
	return items[idx] // no bounds check — panics on out-of-range idx
}
`,
	}
	seedBranchDiffFixture(t, workspaceDir, "main", base, branch, "feature")

	vars := map[string]any{
		"workspace_dir":      workspaceDir,
		"base_ref":           "main",
		"post_to_board":      "false", // contain side-effects: no board writes (string: post_to_board is a tri-state enum since #685)
		"severity_threshold": "low",
		"pr_url":             "", // markdown-only, no forge publish
		"scope_notes":        "Review the diff for correctness and runtime-safety bugs.",
	}
	res := runBotLive(t, liveSpec{
		runIDBase:    "live-review-pr",
		botFile:      "review-pr/main.bot",
		workspaceDir: workspaceDir,
		vars:         vars,
		inputs:       vars,
		timeout:      25 * time.Minute,
	})

	// Reliability invariants.
	assertNodesFinished(t, res.events, "diff_precheck", "fan", "reviewer_claude", "reviewer_gpt", "emit")
	assertSchemaValid(t, res.wf, res.events, "emit")
	emit, ok := lastNodeOutput(res.events, "emit")
	if !ok {
		t.Fatalf("emit produced no output")
	}
	if tf := asFloat(emit["total_findings"]); tf < 1 {
		t.Errorf("expected ≥1 finding for the planted out-of-bounds bug, got total_findings=%v", emit["total_findings"])
	} else {
		t.Logf("review-pr surfaced %.0f finding(s)", tf)
	}

	// Quality: grade the findings report + the diff that was reviewed.
	work := reviewPRWorkProduct(t, workspaceDir, emit)
	assessQuality(t, res, qualityInput{
		kind:          "bot",
		name:          "review-pr",
		persona:       "Revi",
		primaryFamily: "anthropic", // reviewer_claude + emit run on claude_code
		task:          "Review a base..branch diff containing a planted unbounded-index bug; surface findings (read-only, board disabled).",
		workProduct:   work,
	})
}

// reviewPRWorkProduct assembles the artifact to grade: the emitted findings
// report (read from report_path when present) plus the reviewed diff.
func reviewPRWorkProduct(t *testing.T, workspaceDir string, emit map[string]any) string {
	t.Helper()
	var b []byte
	if p, _ := emit["report_path"].(string); p != "" {
		if !filepath.IsAbs(p) {
			p = filepath.Join(workspaceDir, p)
		}
		if data, err := os.ReadFile(p); err == nil {
			b = data
		}
	}
	work := "## review-pr findings report\n" + string(b)
	work += "\n\n## reviewed diff (main..feature)\n" + gitOut(workspaceDir, "--no-pager", "diff", "main..feature")
	return work
}

// asFloat coerces a JSON number (float64) or int to float64; 0 otherwise.
func asFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}
