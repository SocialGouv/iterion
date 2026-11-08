package model

import (
	"context"
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestBuildTask_TheHTTPHostTransportIsNotMintedOnAnUntrustedWorkspace.
//
// The fourth transport of the host capabilities: a SANDBOXED node reaches the
// board and the run store over the per-run listener, with a token the server
// mints for exactly the node's capabilities. The token is the grant. Read at
// the site that mints it — buildTask, where the one resolver answers — on a
// sandboxed node with a live endpoint and a live minter: the trusted control is
// handed both endpoints and a token minted for its capabilities; a fork is
// handed neither, and nothing is minted.
func TestBuildTask_TheHTTPHostTransportIsNotMintedOnAnUntrustedWorkspace(t *testing.T) {
	t.Parallel()
	caps := []string{"board.create", "runs.read"}
	for _, trust := range []store.RunTrust{store.RunTrustDefault, store.RunTrustFork} {
		t.Run(map[store.RunTrust]string{store.RunTrustDefault: "trusted", store.RunTrustFork: "fork"}[trust], func(t *testing.T) {
			var minted [][]string
			e := &ClawExecutor{
				logger:        iterlog.Nop(),
				boardEndpoint: "http://host.docker.internal:4711/api/v1/mcp/board",
				boardRegister: func(c []string, _ string) string {
					minted = append(minted, append([]string(nil), c...))
					return "node-token"
				},
			}
			e.SetSandbox(fakeSandboxRun{})
			e.SetWorkspaceTrust(trust)
			node := &ir.AgentNode{}
			node.ID = "n"
			f := backendFields{id: "n", capabilities: caps, tools: []string{"read_file"}}
			task, err := e.buildTask(context.Background(), node, f, map[string]any{}, delegate.BackendClaudeCode, &nodeBuildSession{})
			if err != nil {
				t.Fatalf("buildTask: %v", err)
			}
			if trust == store.RunTrustDefault {
				if task.BoardHTTPEndpoint == "" || task.RunsHTTPEndpoint == "" || task.BoardRunToken != "node-token" || len(minted) != 1 || !slices.Equal(minted[0], caps) {
					t.Fatalf("the TRUSTED control was not wired (board=%q runs=%q token=%q minted=%v) — this row proves nothing",
						task.BoardHTTPEndpoint, task.RunsHTTPEndpoint, task.BoardRunToken, minted)
				}
				return
			}
			if task.BoardHTTPEndpoint != "" || task.RunsHTTPEndpoint != "" || task.BoardRunToken != "" {
				t.Errorf("a fork's sandboxed node was handed the host transport: board=%q runs=%q token=%q",
					task.BoardHTTPEndpoint, task.RunsHTTPEndpoint, task.BoardRunToken)
			}
			if len(minted) != 0 {
				t.Errorf("a token was minted for a fork's node: %v", minted)
			}
			if len(task.Capabilities) != 0 {
				t.Errorf("a fork's task carries capabilities %v", task.Capabilities)
			}
		})
	}
}

// TestBuildTask_AConnectorNodeIsHandedNoFacadeTransportOnAnUntrustedWorkspace.
//
// The connector facade is the fifth transport of the host capabilities: a
// sandboxed node declaring `connectors` reaches it over the per-run listener
// with a token minted for exactly the node's capabilities. This is the
// endpoint half of ADR-162 decision 6's "the facade is not mounted" for a run
// whose workspace the tenant does not control — the listener half is the
// sandbox gate (workflowHasHostCapability), and the resolution half is
// #1802's own (the connector-catalog bench row is obsolete: the tier rule at
// the resolution IS the floor, and decision 6 withdraws the reads it still
// allows).
func TestBuildTask_AConnectorNodeIsHandedNoFacadeTransportOnAnUntrustedWorkspace(t *testing.T) {
	t.Parallel()
	for _, trust := range []store.RunTrust{store.RunTrustDefault, store.RunTrustFork} {
		t.Run(map[store.RunTrust]string{store.RunTrustDefault: "trusted", store.RunTrustFork: "fork"}[trust], func(t *testing.T) {
			var minted [][]string
			e := &ClawExecutor{
				logger: iterlog.Nop(),
				boardRegister: func(c []string, _ string) string {
					minted = append(minted, append([]string(nil), c...))
					return "node-token"
				},
			}
			e.SetConnectorEndpoint("http://host.docker.internal:4711/api/v1/mcp/connectors")
			e.SetSandbox(fakeSandboxRun{})
			e.SetWorkspaceTrust(trust)
			node := &ir.AgentNode{}
			node.ID = "n"
			f := backendFields{id: "n", capabilities: []string{"connectors"}, tools: []string{"read_file"}}
			task, err := e.buildTask(context.Background(), node, f, map[string]any{}, delegate.BackendClaudeCode, &nodeBuildSession{})
			if err != nil {
				t.Fatalf("buildTask: %v", err)
			}
			if trust == store.RunTrustDefault {
				if task.ConnectorHTTPEndpoint == "" || task.BoardRunToken != "node-token" || len(minted) != 1 {
					t.Fatalf("the TRUSTED control was not wired (facade=%q token=%q minted=%v) — this row proves nothing",
						task.ConnectorHTTPEndpoint, task.BoardRunToken, minted)
				}
				return
			}
			if task.ConnectorHTTPEndpoint != "" || task.BoardRunToken != "" {
				t.Errorf("a fork's connector node was handed the facade transport: endpoint=%q token=%q",
					task.ConnectorHTTPEndpoint, task.BoardRunToken)
			}
			if len(minted) != 0 {
				t.Errorf("a token was minted for a fork's connector node: %v", minted)
			}
		})
	}
}

// TestWatchToolCapability_IsTheValidatorsVocabulary pins the watch
// tool → capability table delegate keeps (it does not import the IR) to the
// names the validator knows, so the registration filter and the call-time
// check cannot drift from the DSL.
func TestWatchToolCapability_IsTheValidatorsVocabulary(t *testing.T) {
	t.Parallel()
	for tool, want := range map[string]string{"subscribe": ir.CapWatchSubscribe, "unsubscribe": ir.CapWatchUnsubscribe} {
		if got := delegate.WatchToolCapability(tool); got != want {
			t.Errorf("watch tool %q opens %q, the validator names %q", tool, got, want)
		}
	}
	if got := delegate.WatchToolCapability("delete_everything"); got != "" {
		t.Errorf("an unknown watch tool maps to %q, want none", got)
	}
}

// TestEffectiveToolNames_ReadsTheSameResolver.
//
// Pre-run admission asks the executor which tool names a node's declaration
// cannot bound (EffectiveToolNames). It reads the node's capabilities through
// the SAME resolver buildTask does, so the surface it reasons about is the one
// the node is handed: a fork's node is not described as holding board tools it
// will never be offered.
func TestEffectiveToolNames_ReadsTheSameResolver(t *testing.T) {
	t.Parallel()
	node := &ir.AgentNode{Capabilities: []string{"board.create"}, Tools: []string{"read_file"}}
	node.ID = "n"
	node.Backend = delegate.BackendClaw
	for _, trust := range []store.RunTrust{store.RunTrustDefault, store.RunTrustFork} {
		e := NewProgramExecutor(&ir.Workflow{})
		e.SetWorkspaceTrust(trust)
		names := e.EffectiveToolNames(node, false)
		board := slices.ContainsFunc(names, func(n string) bool { return delegate.IsIterionMCPTool(n) })
		if want := trust == store.RunTrustDefault; board != want {
			t.Errorf("trust=%q: board tools in the admission surface = %v, want %v (%v)", trust, board, want, names)
		}
	}
}
