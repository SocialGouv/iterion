package cli

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A subbot child's engine is built with WithSharedSandbox and NEITHER sandbox
// tier, so the child resolves its own sandbox from its own workflow. An
// executor that predicted from the PARENT's `--sandbox` flag would therefore
// answer for a run that is not the one about to execute — and the permissive
// direction of that mistake starts a workflow-controlled MCP server beside
// the launcher of a run that asked for isolation.
//
// The two call sites share one helper, which is why no source-shape guard can
// see this: the field IS set, in code a surface where the claim is TRUE also
// uses. So the assertion is behavioural — what the executor's MCP manager is
// armed with — reached through the health check, which is the one caller that
// consults the policy before the engine settles anything.
func TestASubbotChildExecutorDoesNotPredictFromItsParentsSandboxFlags(t *testing.T) {
	// `--sandbox none` on the parent: the permissive prediction.
	opts := RunOptions{Sandbox: "none"}
	// A project-origin server whose command does not exist: if the manager is
	// allowed to start it, the health check fails; if the policy refuses it,
	// the health check skips it and returns nil. Two outcomes, one probe.
	childWf := func() *ir.Workflow {
		return &ir.Workflow{
			Name:  "child",
			Nodes: map[string]ir.Node{},
			ResolvedMCPServers: map[string]*ir.MCPServer{
				"repo": {
					Name: "repo", Origin: string(mcp.OriginProject),
					Transport: ir.MCPTransportStdio,
					Command:   "/nonexistent/iterion-test-mcp-server",
				},
			},
		}
	}

	t.Run("the parent's own run may predict from its flags", func(t *testing.T) {
		exec := buildExecutorForTest(t, opts, tiersMatchTheEngine, childWf())
		if err := healthCheck(t, exec); err == nil {
			t.Error("with --sandbox none the launcher may start the server, so a broken one must be reported")
		}
	})

	t.Run("a child stays fail-closed until its own engine settles", func(t *testing.T) {
		exec := buildExecutorForTest(t, opts, tiersUnknownToAChild, childWf())
		if err := healthCheck(t, exec); err != nil {
			t.Errorf("a child must not act on the parent's tiers; the server should be skipped, got %v", err)
		}
	})
}

func buildExecutorForTest(t *testing.T, opts RunOptions, tiers sandboxTiersClaim, wf *ir.Workflow) any {
	t.Helper()
	storeDir := t.TempDir()
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	exec, err := buildRunExecutor(opts, tiers, wf, s, lineage{runID: "run-1"}, storeDir, iterlog.Nop(), nil, "bot", nil)
	if err != nil {
		t.Fatalf("buildRunExecutor: %v", err)
	}
	return exec
}

// healthCheck reaches the executor's MCP manager through the same optional
// interface `iterion run` uses before dispatching its first node.
func healthCheck(t *testing.T, exec any) error {
	t.Helper()
	hc, ok := exec.(interface {
		MCPHealthCheck(ctx context.Context, servers []string) error
	})
	if !ok {
		t.Fatal("the executor no longer exposes MCPHealthCheck — this test's probe is gone, not the behaviour")
	}
	return hc.MCPHealthCheck(context.Background(), []string{"repo"})
}
