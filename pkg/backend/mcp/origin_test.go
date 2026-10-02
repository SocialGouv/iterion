package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The start policy reads one field, so every path that builds a catalog entry
// has to set it. A copy that drops it does not fail anything visibly — the
// entry simply becomes "unknown", and unknown is refused, so the symptom is a
// server that silently stops working under a sandbox rather than an error
// naming the omission.
func TestOriginTravelsFromEverySource(t *testing.T) {
	dir := t.TempDir()
	mcpJSON := `{"mcpServers":{"fromproject":{"command":"/bin/echo"},
	                            "shadowed":{"command":"/bin/echo"}}}`
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(mcpJSON), 0o600); err != nil {
		t.Fatalf("write .mcp.json: %v", err)
	}

	wf := &ir.Workflow{
		MCPServers: map[string]*ir.MCPServer{
			"fromworkflow": {Name: "fromworkflow", Transport: ir.MCPTransportStdio, Command: "/bin/echo"},
			// Same name as a project entry: "explicit wins" must carry the
			// WINNER's origin, not the one it displaced.
			"shadowed": {Name: "shadowed", Transport: ir.MCPTransportStdio, Command: "/bin/echo"},
		},
	}
	if err := PrepareWorkflow(wf, dir); err != nil {
		t.Fatalf("PrepareWorkflow: %v", err)
	}

	want := map[string]Origin{
		"fromproject":  OriginProject,
		"fromworkflow": OriginWorkflow,
		"shadowed":     OriginWorkflow,
	}
	for name, expected := range want {
		server, ok := wf.ResolvedMCPServers[name]
		if !ok {
			t.Fatalf("server %q missing from the resolved catalog", name)
		}
		if Origin(server.Origin) != expected {
			t.Errorf("server %q: origin %q, want %q", name, server.Origin, expected)
		}
	}
}

// The catalog is cloned on the way in (NewManager), on the way out
// (ServerConfig) and per connection state. A clone that forgets the origin
// downgrades a legitimate operator server to "unknown" somewhere in the
// middle, where no single reader can see it happen.
func TestEveryCloneKeepsTheOrigin(t *testing.T) {
	cfg := &ServerConfig{Name: "s", Origin: OriginPlugin, Transport: TransportStdio, Command: "/bin/echo"}

	if got := cloneServerConfig(cfg).Origin; got != OriginPlugin {
		t.Errorf("cloneServerConfig dropped the origin: %q", got)
	}

	m := NewManager(map[string]*ServerConfig{"s": cfg}, WithStartPolicy(StartOperatorServersOnly))
	out, ok := m.ServerConfig("s")
	if !ok {
		t.Fatal("the server is in the catalog")
	}
	if out.Origin != OriginPlugin {
		t.Errorf("Manager.ServerConfig dropped the origin: %q", out.Origin)
	}
	// The connection state holds its own copy, and it is the one the gate
	// judges.
	state, err := m.state("s")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if state.cfg.Origin != OriginPlugin {
		t.Errorf("the connection state dropped the origin: %q", state.cfg.Origin)
	}
	if err := m.checkStart(state.cfg); err != nil {
		t.Errorf("an operator server must pass the gate: %v", err)
	}
}

// The zero value is the refusal, and it is named: a diagnostic that printed an
// empty string where the origin goes would read as a formatting bug rather
// than as the fact that nobody classified the server.
func TestUnknownOriginIsUntrustedAndNamed(t *testing.T) {
	if OriginUnknown.OperatorControlled() {
		t.Error("the zero value must not be operator-controlled")
	}
	if got := OriginUnknown.String(); got != "unknown" {
		t.Errorf("the zero value renders as %q", got)
	}
	if !OriginPlugin.OperatorControlled() {
		t.Error("plugin origin is the operator's")
	}
	for _, o := range []Origin{OriginProject, OriginWorkflow} {
		if o.OperatorControlled() {
			t.Errorf("%s is workflow-controlled", o)
		}
	}
}
