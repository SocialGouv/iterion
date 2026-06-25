//go:build live

package e2e

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// TestLive_Bot_WhatsNext runs the whats-next bot (Nexie) end-to-end past
// its human gates via the harness auto-resume driver: explore →
// ask_priorities (auto-answered) → propose_roadmap → human_review
// (auto-approved) → emit_action (creates board issues + audit plan) →
// triage gates (auto-answered toward close).
//
// Side-effects are contained to the temp workspace: ITERION_TEST_STORE_DIR
// =workspace points the run store AND the native board at <workspace>/
// .iterion, so emit_action's issues never touch the operator's board.
// Loaded as a bundle so Nexie's skills mirror into .claude/skills.
//
// Reliability invariants: explore + propose_roadmap + emit_action fire;
// emit_action is schema-valid, created_issues is non-empty, and no
// hallucinated assignees. Then the quality panel grades the roadmap +
// issues + value.
//
// Requires: claude CLI (emit_action/triage) + OpenAI (explore/propose).
// Expected: ~15-35 min.
func TestLive_Bot_WhatsNext(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")
	requireOpenAI(t)
	// Isolate run store + native board to the temp workspace so the bot's
	// board writes don't pollute the operator's ~/.iterion board.
	t.Setenv("ITERION_TEST_STORE_DIR", "workspace")

	workspaceDir, err := os.MkdirTemp("", "iterion-whats-next-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Logf("Workspace (persists): %s", workspaceDir)

	// A small but real repo for Nexie to survey.
	seedGoModuleFixture(t, workspaceDir, map[string]string{
		"calc.go": `package calc

// Add returns a + b.
func Add(a, b int) int { return a + b }

// Div returns a / b. TODO: it panics on b == 0 — no guard yet.
func Div(a, b int) int { return a / b }
`,
		"README.md": "# calc\n\nA tiny calculator. No tests yet, no CI.\n",
	})

	vars := map[string]interface{}{
		"workspace_dir": workspaceDir,
		"scope_notes":   "Small Go lib with no tests and an unguarded Div; suggest a short, high-value roadmap.",
		"mode":          "",
	}
	res := runBotLive(t, liveSpec{
		runIDBase:    "live-whats-next",
		bundleDir:    "../bots/whats-next",
		workspaceDir: workspaceDir,
		vars:         vars,
		inputs:       vars,
		timeout:      35 * time.Minute,
		autoResume:   true,
		maxResumes:   14,
	})

	// Reliability invariants.
	assertNodesFinished(t, res.events, "explore", "propose_roadmap", "emit_action")
	assertSchemaValid(t, res.wf, res.events, "emit_action")
	assertOutputFieldsNonEmpty(t, res.events, "emit_action", "created_issues")
	assertNoHallucinatedAssignees(t, res.events, "emit_action")

	emit, _ := lastNodeOutput(res.events, "emit_action")
	assessQuality(t, res, qualityInput{
		kind:          "bot",
		name:          "whats-next",
		persona:       "Nexie",
		primaryFamily: "anthropic", // emit_action/triage on claude_code
		task:          "Survey a tiny untested Go lib and propose a short, high-value roadmap; create board issues for it.",
		workProduct:   whatsNextWorkProduct(emit),
	})
}

// whatsNextWorkProduct renders Nexie's emitted roadmap/issues for grading.
func whatsNextWorkProduct(emit map[string]interface{}) string {
	if emit == nil {
		return "(emit_action produced no output)"
	}
	return fmt.Sprintf("## summary\n%v\n\n## created_issues\n%v\n\n## plan_path\n%v",
		emit["summary"], emit["created_issues"], emit["plan_path"])
}
