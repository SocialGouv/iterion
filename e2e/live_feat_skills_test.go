//go:build live

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLive_Feat_Skills exercises bundle skill mirroring: the skills-demo
// bundle ships skills/demo-token.md, which the runtime mirrors into
// <workspace>/.claude/skills/ at run start (via runtime.WithBundle). The
// agent then reads it and reports the secret token — proving the skill was
// both mirrored (asserted deterministically) AND usable (token echoed).
//
// Requires: ANTHROPIC_API_KEY or OpenAI (the reader is claw openai/gpt-5.5).
// Expected: ~3-6 min.
func TestLive_Feat_Skills(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireOpenAI(t)

	workspaceDir, err := os.MkdirTemp("", "iterion-feat-skills-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	seedGitRepo(t, workspaceDir)

	res := runBotLive(t, liveSpec{
		runIDBase:    "live-feat-skills",
		bundleDir:    "testdata/skills-demo",
		workspaceDir: workspaceDir,
		timeout:      8 * time.Minute,
	})

	// Deterministic: the skill must have been mirrored into the workspace.
	mirrored := filepath.Join(workspaceDir, ".claude", "skills", "demo-token.md")
	if _, err := os.Stat(mirrored); err != nil {
		t.Errorf("expected bundle skill mirrored to %s: %v", mirrored, err)
	}
	// Use: the agent read the mirrored skill and reported its token.
	assertNodesFinished(t, res.events, "reader")
	out, _ := lastNodeOutput(res.events, "reader")
	tok, _ := out["token"].(string)
	if !strings.Contains(tok, "7723") {
		t.Errorf("expected the agent to report the skill's secret token (…7723), got %q", tok)
	} else {
		t.Logf("skill mirrored + used: token=%q", tok)
	}
}
