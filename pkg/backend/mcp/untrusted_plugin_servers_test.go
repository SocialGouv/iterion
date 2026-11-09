package mcp

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestPrepareWorkflow_APluginServerFollowsTheWorkspaceTrust.
//
// A plugin-contributed server is the operator's configuration, booted where
// the agent runs — in the checkout, for claude_code and pi — and its launcher
// reads its own configuration from there. On a workspace its tenant did not
// write it is not resolved; the resolution records what it withheld; a node
// that ACTIVATES it by name is refused with that reason, one that DISABLES it
// is a no-op (the server is already absent — a bot opting out must not fail on
// a fork for it); a misspelt name is still caught when nothing was left unread.
func TestPrepareWorkflow_APluginServerFollowsTheWorkspaceTrust(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	t.Setenv(EnvAutoLoad, "")
	plugin := filepath.Join(home, "plugins", "probe")
	if err := os.MkdirAll(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugin, "plugin.yaml"), []byte(
		"name: probe\nversion: 1.0.0\ndescription: indexer\nschema_version: 1\ndefault_enabled: true\n"+
			"contributes:\n  mcp_servers:\n    - name: plug\n      transport: stdio\n      command: plug-server\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prepare := func(t *testing.T, trust store.RunTrust, cfg *ir.MCPConfig, withMCPJSON bool) (*ir.Workflow, error) {
		t.Helper()
		ws := t.TempDir()
		if withMCPJSON {
			if err := os.WriteFile(filepath.Join(ws, ".mcp.json"), []byte(`{"mcpServers":{"proj":{"command":"true"}}}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		wf := &ir.Workflow{MCP: cfg, Nodes: map[string]ir.Node{"a": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}}}}
		return wf, PrepareWorkflow(wf, ws, ws, trust)
	}

	t.Run("trusted: the plugin's server is resolved and active", func(t *testing.T) {
		wf, err := prepare(t, store.RunTrustDefault, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if wf.ResolvedMCPServers["plug"] == nil || !slices.Contains(wf.ActiveMCPServers, "plug") || (wf.MCPPrepared != nil && len(wf.MCPPrepared.Withdrawn) != 0) {
			t.Fatalf("resolved=%v active=%v withdrawn=%v — this control proves nothing", wf.ResolvedMCPServers, wf.ActiveMCPServers, wf.MCPPrepared)
		}
	})
	t.Run("fork: not resolved, recorded with its plugin", func(t *testing.T) {
		wf, err := prepare(t, store.RunTrustFork, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if wf.ResolvedMCPServers["plug"] != nil || slices.Contains(wf.ActiveMCPServers, "plug") {
			t.Errorf("a fork resolved the plugin's server: resolved=%v active=%v", wf.ResolvedMCPServers, wf.ActiveMCPServers)
		}
		if want := []ir.WithdrawnMCPServer{{Name: "plug", Source: "plugin probe"}}; wf.MCPPrepared == nil || !slices.Equal(wf.MCPPrepared.Withdrawn, want) {
			t.Errorf("preparation = %+v, want withdrawn %v", wf.MCPPrepared, want)
		}
	})
	t.Run("fork: activating it by name is refused with the reason", func(t *testing.T) {
		_, err := prepare(t, store.RunTrustFork, &ir.MCPConfig{Servers: []string{"plug"}}, false)
		if !errors.Is(err, ErrWithdrawnMCPServer) || !strings.Contains(err.Error(), "plugin probe") {
			t.Fatalf("err = %v, want ErrWithdrawnMCPServer naming the plugin", err)
		}
	})
	t.Run("fork: disabling it is a no-op", func(t *testing.T) {
		if _, err := prepare(t, store.RunTrustFork, &ir.MCPConfig{Disable: []string{"plug"}}, false); err != nil {
			t.Fatalf("disabling a withdrawn server failed the fork run: %v", err)
		}
	})
	t.Run("trusted: disabling it still removes it", func(t *testing.T) {
		wf, err := prepare(t, store.RunTrustDefault, &ir.MCPConfig{Disable: []string{"plug"}}, false)
		if err != nil || slices.Contains(wf.ActiveMCPServers, "plug") {
			t.Fatalf("err=%v active=%v", err, wf.ActiveMCPServers)
		}
	})
	t.Run("fork: a NODE-level disable of it is a no-op too", func(t *testing.T) {
		ws := t.TempDir()
		wf := &ir.Workflow{Nodes: map[string]ir.Node{"a": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, MCP: &ir.MCPConfig{Disable: []string{"plug"}}}}}
		if err := PrepareWorkflow(wf, ws, ws, store.RunTrustFork); err != nil {
			t.Fatalf("a node disabling a withdrawn server failed the fork run: %v", err)
		}
	})
	t.Run("fork: a plugin server the workflow's own declaration shadows is not withdrawn", func(t *testing.T) {
		ws := t.TempDir()
		wf := &ir.Workflow{
			MCPServers: map[string]*ir.MCPServer{"plug": {Name: "plug", Transport: ir.MCPTransportStdio, Command: "own-server"}},
			Nodes:      map[string]ir.Node{"a": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}}},
		}
		if err := PrepareWorkflow(wf, ws, ws, store.RunTrustFork); err != nil {
			t.Fatal(err)
		}
		if wf.MCPPrepared != nil && len(wf.MCPPrepared.Withdrawn) != 0 {
			t.Errorf("announced as withdrawn: %+v — the workflow's own server of that name runs", wf.MCPPrepared.Withdrawn)
		}
		if got := wf.ResolvedMCPServers["plug"]; got == nil || got.Command != "own-server" {
			t.Errorf("the workflow's own server did not resolve: %+v", got)
		}
	})
	t.Run("fork: a misspelt name is still caught when nothing was left unread", func(t *testing.T) {
		_, err := prepare(t, store.RunTrustFork, &ir.MCPConfig{Disable: []string{"plgu"}}, false)
		if err == nil || !strings.Contains(err.Error(), `unknown MCP server "plgu"`) {
			t.Fatalf("err = %v, want the unknown-server error", err)
		}
	})
	t.Run("fork: the checkout's .mcp.json is recorded, and a server it holds is withdrawn", func(t *testing.T) {
		wf, err := prepare(t, store.RunTrustFork, &ir.MCPConfig{Disable: []string{"proj"}}, true)
		if err != nil {
			t.Fatalf("disabling a server of the withheld .mcp.json failed the fork run: %v", err)
		}
		if wf.MCPPrepared == nil || !slices.Contains(wf.MCPPrepared.Withdrawn, ir.WithdrawnMCPServer{Source: "the checkout's .mcp.json"}) {
			t.Errorf("preparation = %+v, want the withheld .mcp.json recorded", wf.MCPPrepared)
		}
		_, err = prepare(t, store.RunTrustFork, &ir.MCPConfig{Servers: []string{"proj"}}, true)
		if !errors.Is(err, ErrWithdrawnMCPServer) || !strings.Contains(err.Error(), "(the checkout's .mcp.json)") {
			t.Fatalf("err = %v, want ErrWithdrawnMCPServer naming the checkout's .mcp.json", err)
		}
	})
}
