package bots_test

import (
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

func TestCopilotDebugBudgetPolicyMatchesRealConversationState(t *testing.T) {
	m, err := bundle.LoadManifest(filepath.Join("copilot", bundle.ManifestFile))
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m == nil || m.Chat == nil {
		t.Fatal("Copi manifest has no chat surface")
	}
	if !m.Chat.UnlimitedWorkflowForLaunch(map[string]string{"mode": "debug"}) {
		t.Fatal("Copi debug launch did not activate unlimited workflow budget")
	}
	if !m.Chat.UnlimitedWorkflowForOutputs(map[string]map[string]any{"compose": {"mode": "debug"}}) {
		t.Fatal("Copi compose.mode=debug did not activate on resume")
	}
	if m.Chat.UnlimitedWorkflowForLaunch(map[string]string{"mode": "info"}) {
		t.Fatal("Copi info mode unexpectedly activates unlimited workflow budget")
	}
}
