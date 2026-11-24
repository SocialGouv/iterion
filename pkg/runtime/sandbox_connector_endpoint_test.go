package runtime

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// The connector endpoint is the engine's to give only when the host MCP
// handler SERVES the facade (adverse review MED-1, #2632): the cloud runner
// pod wires its mux with WithBoardMCPScoped(h, false) — board + runs only —
// and a sandboxed `connectors`-capability node must then keep the documented
// degradation (no ConnectorHTTPEndpoint → the delegate warns, tools
// disabled) instead of an AlwaysLoad MCP server 404ing at every call. The
// listener itself still starts on the connectors capability: it serves the
// board/runs routes to any other node of the same run, and a
// connectors-only workflow's listener is simply unused — harmless and
// logged at sandbox start.

// hostMCPCapturingExecutor records the endpoints the engine pushes after
// the sandbox starts.
type hostMCPCapturingExecutor struct {
	*stubExecutor
	mu                               sync.Mutex
	boardEndpoint, connectorEndpoint string
	boardSet, connectorSet           bool
}

func (e *hostMCPCapturingExecutor) SetBoardEndpoint(endpoint string) {
	e.mu.Lock()
	e.boardEndpoint, e.boardSet = endpoint, true
	e.mu.Unlock()
}

func (e *hostMCPCapturingExecutor) SetConnectorEndpoint(endpoint string) {
	e.mu.Lock()
	e.connectorEndpoint, e.connectorSet = endpoint, true
	e.mu.Unlock()
}

func (e *hostMCPCapturingExecutor) seen() (board string, boardSet bool, connector string, connectorSet bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.boardEndpoint, e.boardSet, e.connectorEndpoint, e.connectorSet
}

func connectorsOnlyWorkflow() *ir.Workflow {
	agent := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "agent"},
		LLMFields: ir.LLMFields{Backend: "claude_code"},
	}
	agent.Capabilities = []string{connection.CapConnectors}
	return &ir.Workflow{
		Name: "conn_scope", Entry: "agent",
		Nodes: map[string]ir.Node{
			"agent": agent,
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges:   []*ir.Edge{{From: "agent", To: "done"}},
		Schemas: map[string]*ir.Schema{}, Prompts: map[string]*ir.Prompt{},
		Vars: map[string]*ir.Var{}, Loops: map[string]*ir.Loop{},
		Sandbox: &ir.SandboxSpec{Mode: "inline", Image: "example.invalid/iterion-sandbox:test", HostState: "none"},
	}
}

func TestConnectorEndpointFollowsWhatTheHandlerServes(t *testing.T) {
	t.Setenv("ITERION_MODE", "local") // pin the driver's preference walk, as workingHostRegistry explains
	for _, tc := range []struct {
		name          string
		scoped        bool // WithBoardMCPScoped(h, false) — the cloud runner pod
		wantConnector bool
	}{
		{"scoped without the facade (cloud runner pod)", true, false},
		{"the server's full mux", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := tmpStore(t)
			run, err := s.CreateRun(ctx, "run-conn-scope", "conn_scope", nil)
			if err != nil {
				t.Fatal(err)
			}
			exec := &hostMCPCapturingExecutor{stubExecutor: newStubExecutor()}
			handler := http.NewServeMux()
			opts := []EngineOption{
				WithLogger(iterlog.Nop()),
				WithWorkDir(t.TempDir()),
				WithSandboxDrivers(map[string]sandbox.DriverConstructor{
					"docker": func() (sandbox.Driver, error) { return &podDriver{root: t.TempDir()}, nil },
				}),
			}
			if tc.scoped {
				opts = append(opts, WithBoardMCPScoped(handler, false))
			} else {
				opts = append(opts, WithBoardMCP(handler))
			}
			eng := New(connectorsOnlyWorkflow(), s, exec, opts...)
			if runErr := eng.Run(ctx, run.ID, nil); runErr != nil {
				t.Fatalf("run: %v", runErr)
			}
			board, boardSet, connector, connectorSet := exec.seen()
			// The listener starts either way (the connectors capability is a
			// host capability) — what differs is whether the executor is told
			// the facade is on it.
			if !boardSet || board == "" {
				t.Fatal("the board endpoint was not pushed — the per-run listener must start on a host-capability workflow")
			}
			if connectorSet != tc.wantConnector {
				t.Fatalf("SetConnectorEndpoint called = %v, want %v", connectorSet, tc.wantConnector)
			}
			if tc.wantConnector && !strings.HasSuffix(connector, "/api/v1/mcp/connectors") {
				t.Fatalf("connector endpoint = %q, want the facade path on the listener's base", connector)
			}
			if !tc.wantConnector && connector != "" {
				t.Fatalf("connector endpoint = %q, want empty — the pod serves board/runs only", connector)
			}
		})
	}
}
