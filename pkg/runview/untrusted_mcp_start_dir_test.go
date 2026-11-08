package runview

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestBuildMCPManager_AStdioServerStartsFromTheRootOnAnUntrustedWorkspace.
//
// The in-process MCP manager — the health check, claw's own tool calls —
// starts a stdio server with iterion's working directory unless told
// otherwise, and an `iterion run` a node starts runs IN the checkout: a
// launcher there reads its configuration from the fork's tree. On a catalog
// resolved for an untrusted workspace the server starts from the root; on a
// trusted one, where it always did. The server records where it started.
func TestBuildMCPManager_AStdioServerStartsFromTheRootOnAnUntrustedWorkspace(t *testing.T) {
	here, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		name  string
		trust store.RunTrust
		want  string
	}{
		{"trusted (the control)", store.RunTrustDefault, here},
		{"fork", store.RunTrustFork, "/"},
	} {
		t.Run(row.name, func(t *testing.T) {
			rec := filepath.Join(t.TempDir(), "cwd")
			wf := &ir.Workflow{
				ResolvedMCPServers: map[string]*ir.MCPServer{"probe": {
					Name: "probe", Transport: ir.MCPTransportStdio, Command: "/bin/sh",
					Args: []string{"-c", "pwd > '" + rec + "'; exit 1"},
				}},
				MCPPrepared: &ir.MCPPreparation{Trust: string(row.trust)},
			}
			mgr, _, err := buildMCPManager(wf, t.TempDir(), iterlog.Nop(), mcp.StartAllServers)
			if err != nil || mgr == nil {
				t.Fatalf("buildMCPManager: mgr=%v err=%v", mgr, err)
			}
			defer func() { _ = mgr.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			_ = mgr.HealthCheck(ctx, []string{"probe"}) // the probe is no MCP server: its start is the row
			got, err := os.ReadFile(rec)
			if err != nil {
				t.Fatalf("the server never started (%v) — the row proves nothing", err)
			}
			if strings.TrimSpace(string(got)) != row.want {
				t.Errorf("the server started in %q, want %q", strings.TrimSpace(string(got)), row.want)
			}
		})
	}
}
