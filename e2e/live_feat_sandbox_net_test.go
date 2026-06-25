//go:build live

package e2e

import (
	"os"
	"testing"
	"time"
)

// TestLive_Feat_Sandbox_Network exercises the sandbox network allowlist: a
// tool-only workflow runs inside iterion-sandbox-full with
// network.mode=allowlist + the iterion-default preset, and curls two hosts.
// github (a git host in the preset) should be reachable; example.com (not
// in the preset) must be BLOCKED by the host CONNECT proxy — the egress
// policy actually enforcing.
//
// No LLM (deterministic tool node). Requires docker + the
// iterion-sandbox-full:edge image (skipped otherwise). Expected: ~2-5 min.
func TestLive_Feat_Sandbox_Network(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireDockerImage(t, "ghcr.io/socialgouv/iterion-sandbox-full:edge")

	workspaceDir, err := os.MkdirTemp("", "iterion-feat-sandbox-net-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	seedGitRepo(t, workspaceDir)

	res := runBotLive(t, liveSpec{
		runIDBase:    "live-feat-sandbox-net",
		botFile:      "feat_sandbox_net.bot",
		workspaceDir: workspaceDir,
		timeout:      10 * time.Minute,
		withWorkDir:  true, // sandbox bind-mounts the workdir
	})

	assertNodesFinished(t, res.events, "net_probe")
	// The egress to a non-allow-listed host must be blocked by the proxy.
	if !eventDataMentions(res.events, "EXAMPLE_BLOCKED") {
		t.Errorf("expected egress to example.com (not in the allowlist preset) to be BLOCKED by the CONNECT proxy")
	} else {
		t.Logf("sandbox network allowlist blocked the off-policy egress (example.com)")
	}
	// A preset git host should remain reachable (informational — a proxy/
	// preset change would surface here without failing the egress assertion).
	if eventDataMentions(res.events, "GITHUB_OK") {
		t.Logf("preset git host (github.com) reachable, as expected")
	} else {
		t.Logf("note: github.com not reachable — verify the iterion-default preset/proxy")
	}
}
