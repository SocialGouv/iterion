package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// `iterion run` compiles for the trust it launches with (cliLaunchTrust): the
// MCP catalog the run's executor boots is resolved for it, and the
// resolution records it.
func TestResolveWorkflow_CompilesForTheLaunchTrust(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "w.bot")
	if err := os.WriteFile(path, []byte("\nworkflow w:\n  entry: done\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, trust := range []store.RunTrust{store.RunTrustDefault, store.RunTrustFork} {
		wf, _, _, _, _, cleanup, err := resolveWorkflow(RunOptions{File: path}, trust)
		if err != nil {
			t.Fatalf("resolveWorkflow(%q): %v", trust, err)
		}
		_ = cleanup()
		if wf.MCPPrepared == nil || store.RunTrust(wf.MCPPrepared.Trust) != trust {
			t.Errorf("resolveWorkflow(%q) resolved the catalog for %+v", trust, wf.MCPPrepared)
		}
	}
}
