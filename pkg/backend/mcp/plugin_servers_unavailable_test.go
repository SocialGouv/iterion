package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/plugin"
)

// A plugin registry that cannot load takes EVERY plugin MCP server off every
// node. PrepareWorkflow reports it through an optional logger — which only
// the run path passes — so the read-only analyses have to be able to ask the
// question themselves, and get the reason rather than a bare bool.
func TestABrokenPluginRegistryIsReportableWithoutALogger(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "plugins.yaml"),
		[]byte("enabled: [this is not\n  valid: yaml: at all\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The premise: the registry really does refuse this file. Without it the
	// assertion below would pass on any change that made the helper always
	// return an error, and on any change that made it always return nil.
	if _, err := plugin.Load(); err == nil {
		t.Fatal("premise broken: the registry accepted a malformed plugins.yaml, so this test proves nothing")
	}

	err := PluginServersUnavailable()
	if err == nil {
		t.Fatal("a registry that cannot load must be reportable — silence reads as \"no plugins are enabled\"")
	}
	if !strings.Contains(err.Error(), "plugin registry") {
		t.Errorf("the reason must name what failed: %v", err)
	}
}

// And a healthy registry says nothing, so the diagnostic means something
// when it appears.
func TestAHealthyPluginRegistryIsSilent(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	if err := PluginServersUnavailable(); err != nil {
		t.Errorf("an empty home is a perfectly good registry: %v", err)
	}
}
