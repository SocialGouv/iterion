//go:build unix

package mcp

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestPrepareWorkflow_TheCheckoutsServerNamesTellAWithdrawnServerFromATypo.
//
// On a workspace its tenant did not write, the checkout's .mcp.json is not
// autoloaded — its entries carry the command the agent would boot. Its server
// NAMES alone are read, so a bot activating one of them is refused with the
// floor's reason while a name the file does not hold stays the typo it is on
// every other run. A file that cannot be read that way (a link, a fifo, over
// the bound, not that JSON) leaves its names unknown: every name the catalog
// lacks may be one of its. The announcement names the file, never its names.
func TestPrepareWorkflow_TheCheckoutsServerNamesTellAWithdrawnServerFromATypo(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	t.Setenv(EnvAutoLoad, "")
	const valid = `{"mcpServers":{"proj":{"command":"true"}}}`
	prepare := func(t *testing.T, trust store.RunTrust, plant func(ws string), declared map[string]*ir.MCPServer, cfg *ir.MCPConfig) (*ir.Workflow, error) {
		t.Helper()
		ws := t.TempDir()
		if plant != nil {
			plant(ws)
		}
		wf := &ir.Workflow{MCP: cfg, MCPServers: declared, Nodes: map[string]ir.Node{"a": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}}}}
		done := make(chan error, 1)
		go func() { done <- PrepareWorkflow(wf, ws, ws, trust) }()
		select {
		case err := <-done:
			return wf, err
		case <-time.After(10 * time.Second):
			t.Fatal("PrepareWorkflow is parked on the checkout's .mcp.json")
			return nil, nil
		}
	}
	file := func(body string) func(string) {
		return func(ws string) {
			if err := os.WriteFile(filepath.Join(ws, ".mcp.json"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	announced := func(wf *ir.Workflow) bool {
		return wf != nil && wf.MCPPrepared != nil && slices.Contains(wf.MCPPrepared.Withdrawn, ir.WithdrawnMCPServer{Source: checkoutMCPJSON})
	}

	t.Run("trusted: the file's server resolves (the control)", func(t *testing.T) {
		wf, err := prepare(t, store.RunTrustDefault, file(valid), nil, &ir.MCPConfig{Servers: []string{"proj"}})
		if err != nil || wf.ResolvedMCPServers["proj"] == nil {
			t.Fatalf("err=%v resolved=%v — this control proves nothing", err, wf.ResolvedMCPServers)
		}
	})
	t.Run("fork: activating a server the file holds is refused with the floor's reason", func(t *testing.T) {
		wf, err := prepare(t, store.RunTrustFork, file(valid), nil, &ir.MCPConfig{Servers: []string{"proj"}})
		if !errors.Is(err, ErrWithdrawnMCPServer) || !strings.Contains(err.Error(), "(the checkout's .mcp.json)") {
			t.Fatalf("err = %v, want ErrWithdrawnMCPServer naming the checkout's .mcp.json", err)
		}
		if wf.ResolvedMCPServers["proj"] != nil {
			t.Errorf("a fork resolved the checkout's server: %+v", wf.ResolvedMCPServers["proj"])
		}
	})
	t.Run("fork: a name the file does not hold is a typo", func(t *testing.T) {
		_, err := prepare(t, store.RunTrustFork, file(valid), nil, &ir.MCPConfig{Servers: []string{"porj"}})
		if err == nil || errors.Is(err, ErrWithdrawnMCPServer) || !strings.Contains(err.Error(), `unknown MCP server "porj"`) {
			t.Fatalf("err = %v, want the unknown-server error", err)
		}
		_, err = prepare(t, store.RunTrustFork, file(valid), nil, &ir.MCPConfig{Disable: []string{"porj"}})
		if err == nil || !strings.Contains(err.Error(), `unknown MCP server "porj"`) {
			t.Fatalf("disable: err = %v, want the unknown-server error", err)
		}
	})
	t.Run("fork: disabling a server the file holds is a no-op, and the file is announced by name only", func(t *testing.T) {
		wf, err := prepare(t, store.RunTrustFork, file(valid), nil, &ir.MCPConfig{Disable: []string{"proj"}})
		if err != nil {
			t.Fatal(err)
		}
		if !announced(wf) {
			t.Errorf("preparation = %+v, want the checkout's .mcp.json announced", wf.MCPPrepared)
		}
		for _, w := range wf.MCPPrepared.Withdrawn {
			if w.Name == "proj" {
				t.Errorf("the announcement carries a name out of the checkout: %+v", w)
			}
		}
	})
	t.Run("fork: a file with no server, or only one the workflow declares itself, withholds nothing", func(t *testing.T) {
		wf, err := prepare(t, store.RunTrustFork, file(`{"mcpServers":{}}`), nil, nil)
		if err != nil || announced(wf) {
			t.Errorf("empty file: err=%v preparation=%+v", err, wf.MCPPrepared)
		}
		own := map[string]*ir.MCPServer{"proj": {Name: "proj", Transport: ir.MCPTransportStdio, Command: "own-server"}}
		wf, err = prepare(t, store.RunTrustFork, file(valid), own, nil)
		if err != nil || announced(wf) {
			t.Errorf("shadowed: err=%v preparation=%+v", err, wf.MCPPrepared)
		}
	})

	big := `{"mcpServers":{"proj":{"command":"true"}},"pad":"` + strings.Repeat("x", maxCheckoutMCPJSON) + `"}`
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, unreadable := range []struct {
		name  string
		plant func(ws string)
	}{
		{"not JSON", file(`{"mcpServers":`)},
		{"over the bound", file(big)},
		{"a link to a readable file", func(ws string) {
			if err := os.Symlink(target, filepath.Join(ws, ".mcp.json")); err != nil {
				t.Fatal(err)
			}
		}},
		{"a fifo", func(ws string) {
			if err := syscall.Mkfifo(filepath.Join(ws, ".mcp.json"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run("fork, unreadable ("+unreadable.name+"): every unknown name may be one of its", func(t *testing.T) {
			wf, err := prepare(t, store.RunTrustFork, unreadable.plant, nil, &ir.MCPConfig{Servers: []string{"porj"}})
			if !errors.Is(err, ErrWithdrawnMCPServer) || !strings.Contains(err.Error(), ".mcp.json, not read") {
				t.Fatalf("err = %v, want ErrWithdrawnMCPServer naming the unread .mcp.json", err)
			}
			wf, err = prepare(t, store.RunTrustFork, unreadable.plant, nil, nil)
			if err != nil || !announced(wf) {
				t.Errorf("err=%v preparation=%+v, want the unread file announced", err, wf.MCPPrepared)
			}
		})
	}
}
