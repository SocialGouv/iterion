package runtime

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// infoCapturingDriver passes Prepare and records the RunInfo the engine hands
// Start, then refuses to start.
type infoCapturingDriver struct{ info *sandbox.RunInfo }

type capturedPrepared struct{}

func (capturedPrepared) DriverName() string { return "docker" }

func (d *infoCapturingDriver) Name() string { return "docker" }
func (d *infoCapturingDriver) Capabilities() sandbox.Capabilities {
	// SupportsNetworkPolicy: the fork rows must reach Start to witness the
	// listener wiring, so the fixture claims the network floor an untrusted
	// start requires (ADR-162 decision 7).
	return sandbox.Capabilities{SupportsImage: true, SupportsMounts: true, SupportsNetworkPolicy: true}
}
func (d *infoCapturingDriver) Prepare(context.Context, sandbox.Spec) (sandbox.PreparedSpec, error) {
	return capturedPrepared{}, nil
}
func (d *infoCapturingDriver) Start(_ context.Context, _ sandbox.PreparedSpec, info sandbox.RunInfo) (sandbox.Run, error) {
	d.info = &info
	return nil, errCanaryStop
}

// TestUntrustedWorkspace_TheSandboxOpensNoHostCapabilityListener.
//
// The sandbox decides BEFORE the container starts whether to open the per-run
// board/runs listener and to give the container the host-gateway alias that
// reaches it. It read the workflow's DECLARED capabilities, so a fork's
// container was handed a route to a host listener for tools none of its nodes
// may hold. It reads them through the same resolver the executor uses: the
// trusted control is given the alias, a fork is not.
func TestUntrustedWorkspace_TheSandboxOpensNoHostCapabilityListener(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ITERION_HOME", t.TempDir())
	t.Setenv("ITERION_MODE", "local")
	for _, row := range []struct {
		name  string
		trust store.RunTrust
		want  bool
	}{
		{"trusted", store.RunTrustDefault, true},
		{"fork", store.RunTrustFork, false},
	} {
		// `connectors` rides the same listener and the same alias (the
		// facade is a route on the board mux), so it is held to the same
		// gate: ADR-162 decision 6's "the facade is not mounted" for a run
		// whose workspace the tenant does not control starts here — no
		// listener, no alias — and ends at applyConnectorEndpoint, which
		// hands such a node no endpoint and no token.
		for _, caps := range [][]string{{"board.create"}, {"runs.read"}, {connection.CapConnectors}} {
			t.Run(row.name+"/"+caps[0], func(t *testing.T) {
				d := &infoCapturingDriver{}
				p := SandboxParams{
					Workflow: &ir.Workflow{
						Name:         "wf",
						Capabilities: caps,
						Nodes:        map[string]ir.Node{"a": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}}},
						Sandbox:      &ir.SandboxSpec{Mode: "inline", Image: "example.invalid/sbx:test"},
					},
					RunID:           "run-listener",
					WorkspacePath:   t.TempDir(),
					Trust:           row.trust,
					BoardMCPHandler: http.NotFoundHandler(),
					Drivers:         map[string]sandbox.DriverConstructor{"docker": func() (sandbox.Driver, error) { return d, nil }},
					Logger:          iterlog.Nop(),
					EmitEvent:       func(store.EventType, map[string]any) error { return nil },
				}
				if _, err := resolveAndStartSandbox(context.Background(), p); !errors.Is(err, errCanaryStop) || d.info == nil {
					t.Fatalf("resolveAndStartSandbox = %v, want the canary driver's refusal at Start — the wiring did not reach Start, so this row proves nothing", err)
				}
				if got := d.info.HostGatewayAlias; got != row.want {
					t.Fatalf("HostGatewayAlias = %v, want %v (trust=%q, declared %v)", got, row.want, row.trust, caps)
				}
			})
		}
	}
}
