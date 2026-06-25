//go:build live

package e2e

import (
	"os"
	"testing"
	"time"
)

// TestLive_Feat_Ultracode exercises reasoning_effort: ultracode via
// examples/ultracode/sample.bot: a claude_code implementer on
// claude-opus-4-8 that adds a CHANGELOG entry. ultracode = xhigh on the
// wire + a ## Workflow Orchestration prompt section + the auto-added
// `agent` subagent tool (reliable only on 4.8). The feature works
// end-to-end when the node runs on opus without error and produces a real
// edit.
//
// Requires: claude CLI + an Anthropic credential (opus-4-8). Expected:
// ~5-15 min.
func TestLive_Feat_Ultracode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")
	requireOpus48(t)

	workspaceDir, err := os.MkdirTemp("", "iterion-feat-ultracode-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	seedGoModuleFixture(t, workspaceDir, map[string]string{
		"main.go": "package main\n\nfunc main() {}\n",
	})

	res := runBotLive(t, liveSpec{
		runIDBase:    "live-feat-ultracode",
		botFile:      "ultracode/sample.bot",
		workspaceDir: workspaceDir,
		timeout:      15 * time.Minute,
	})

	assertNodesFinished(t, res.events, "implementer")
	// The implementer edits files (no commit step in this minimal bot), so
	// the working tree must show a change (e.g. a new CHANGELOG).
	if dirty := gitOut(workspaceDir, "status", "--porcelain"); len(dirty) == 0 {
		t.Errorf("expected the ultracode implementer to edit the working tree (no changes detected)")
	} else {
		t.Logf("ultracode implementer changed:\n%s", dirty)
	}

	assessQuality(t, res, qualityInput{
		kind:          "feature",
		name:          "ultracode",
		primaryFamily: "anthropic",
		task:          "Run a claude-opus-4-8 implementer at reasoning_effort: ultracode to add a CHANGELOG entry.",
		workProduct:   gitArtifactEvidence(t, workspaceDir),
	})
}
