package model

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestResolveTaskMCPServers_AForksStdioServerStartsOutsideTheCheckout.
//
// A CLI backend spawns the stdio MCP servers it is handed in ITS working
// directory, the checkout, and a server's launcher reads its configuration
// from there. On a workspace its tenant did not write, what the executor hands
// over starts from the filesystem root instead. Executed, not inspected: the
// projected command is run from inside a checkout and reports where the server
// itself started. The trusted row is the control — unchanged, it starts in the
// checkout.
func TestResolveTaskMCPServers_AForksStdioServerStartsOutsideTheCheckout(t *testing.T) {
	for _, row := range []struct {
		name  string
		trust store.RunTrust
		root  bool
	}{
		{"trusted (control)", store.RunTrustDefault, false},
		{"fork", store.RunTrustFork, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			e := &ClawExecutor{
				logger: iterlog.Nop(),
				mcpManager: mcp.NewManager(map[string]*mcp.ServerConfig{
					"srv": {Name: "srv", Transport: mcp.TransportStdio, Command: "/bin/sh", Args: []string{"-c", `printf '%s' "$PWD" "$1"`, "sh", "arg-kept"}},
				}),
			}
			e.SetWorkspaceTrust(row.trust)
			servers := e.resolveTaskMCPServers([]string{"srv"})
			if len(servers) != 1 {
				t.Fatalf("servers = %+v", servers)
			}
			checkout := t.TempDir()
			cmd := exec.Command(servers[0].Command, servers[0].Args...)
			cmd.Dir = checkout
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("run the projected server: %v", err)
			}
			got := string(out)
			if !strings.HasSuffix(got, "arg-kept") {
				t.Errorf("the server's own arguments did not reach it: %q", got)
			}
			pwd := strings.TrimSuffix(got, "arg-kept")
			if row.root && pwd != "/" {
				t.Errorf("a fork's server started in %q, want /", pwd)
			}
			if !row.root && pwd != checkout {
				t.Errorf("the trusted server started in %q, want the checkout %q — the control proves nothing", pwd, checkout)
			}
		})
	}
}
