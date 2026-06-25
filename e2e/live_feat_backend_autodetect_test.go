//go:build live

package e2e

import (
	"os"
	"testing"
	"time"
)

// TestLive_Feat_BackendAutodetect exercises backend auto-detection: an
// agent that declares neither backend nor model forces the runtime
// resolver to probe host credentials, pick a backend, and substitute a
// model — then run successfully.
//
// Requires: claude CLI (the resolver's first preference). Expected:
// ~2-5 min.
func TestLive_Feat_BackendAutodetect(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")

	workspaceDir, err := os.MkdirTemp("", "iterion-feat-autodetect-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	seedGitRepo(t, workspaceDir)

	res := runBotLive(t, liveSpec{
		runIDBase:    "live-feat-backend-autodetect",
		botFile:      "feat_backend_autodetect.bot",
		workspaceDir: workspaceDir,
		timeout:      5 * time.Minute,
	})

	assertNodesFinished(t, res.events, "greeter")
	assertOutputFieldsNonEmpty(t, res.events, "greeter", "greeting")
}
