package model

import (
	"context"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The connector endpoint is its own push (SetConnectorEndpoint), NOT a
// derivation from the board endpoint: a host can serve board/runs without
// the connector facade — the cloud runner pod's mux does exactly that
// (#2632). A sandboxed `connectors`-cap node on such a host must carry NO
// ConnectorHTTPEndpoint, so the delegate keeps its documented degradation
// (loud warning, tools disabled) instead of registering an AlwaysLoad MCP
// server that 404s at every call (adverse review MED-1).

// endpointRecordingBackend captures the connector endpoint the task carried.
type endpointRecordingBackend struct {
	mu        sync.Mutex
	connector string
}

func (b *endpointRecordingBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.mu.Lock()
	b.connector = task.ConnectorHTTPEndpoint
	b.mu.Unlock()
	return delegate.Result{Output: map[string]any{"ok": true}}, nil
}

func (b *endpointRecordingBackend) seen() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.connector
}

func TestConnectorEndpointIsNotDerivedFromTheBoardEndpoint(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name              string
		connectorEndpoint string
		wantConnector     bool
	}{
		{"host without the facade (cloud runner pod)", "", false},
		{"host serving the facade (server mux)", "http://127.0.0.1:1/api/v1/mcp/connectors", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &endpointRecordingBackend{}
			reg := delegate.NewRegistry()
			reg.Register("crossing", b)
			e := NewClawExecutor(NewRegistry(),
				&ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{}},
				WithBackendRegistry(reg),
				WithRetryPolicy(RetryPolicy{MaxAttempts: 1}),
				WithBoardRegister(func([]string, string) string { return "tok" }),
				WithBoardRevoke(func(string) {}),
			)
			e.SetSandbox(unmetSandboxRun{})
			e.SetBoardEndpoint("http://127.0.0.1:1/api/v1/mcp/board")
			if tc.connectorEndpoint != "" {
				e.SetConnectorEndpoint(tc.connectorEndpoint)
			}

			if _, err := e.executeBackend(context.Background(), connectorsNode(), map[string]any{}); err != nil {
				t.Fatalf("executeBackend: %v", err)
			}
			connector := b.seen()
			if tc.wantConnector && connector != tc.connectorEndpoint {
				t.Fatalf("ConnectorHTTPEndpoint = %q, want the pushed %q", connector, tc.connectorEndpoint)
			}
			if !tc.wantConnector && connector != "" {
				t.Fatalf("ConnectorHTTPEndpoint = %q, want empty — the facade is not served by this host, the endpoint must not be derived from the board endpoint", connector)
			}
		})
	}
}
