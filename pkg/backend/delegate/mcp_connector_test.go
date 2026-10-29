package delegate

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/connection"
)

// The `connectors` capability is the DSL spelling that wires a node to the
// connector MCP facade (#2758). The predicate reads the ONE source of the
// string (connection.CapConnectors), so a typo'd cap stays what C080 warns
// about — a warning, passed through — rather than a silently dead wire.
func TestHasConnectorsCapability(t *testing.T) {
	if !HasConnectorsCapability([]string{"board.read", connection.CapConnectors}) {
		t.Error("a cap list holding connectors must answer true")
	}
	if HasConnectorsCapability([]string{"board.read"}) {
		t.Error("board caps alone must not wire the facade")
	}
	if HasConnectorsCapability([]string{"connectors.read"}) {
		t.Error("a namespaced lookalike is not the capability — the facade is all-or-nothing per node")
	}
	if HasConnectorsCapability(nil) {
		t.Error("no caps, no facade")
	}
}

// The discovery pair joins a RESTRICTED tool set, under the facade's own
// server name — and nothing joins when the capability is absent.
func TestConnectorToolsFor(t *testing.T) {
	tools := ConnectorToolsFor([]string{connection.CapConnectors})
	if len(tools) != 2 {
		t.Fatalf("tools = %v, want the discovery pair", tools)
	}
	for _, name := range tools {
		if got := "mcp__" + connectorMCPServerName + "__"; len(name) <= len(got) || name[:len(got)] != got {
			t.Errorf("tool %q is not under the facade's server name %q", name, connectorMCPServerName)
		}
	}
	if got := ConnectorToolsFor([]string{"board.read"}); got != nil {
		t.Errorf("no capability, no tools: %v", got)
	}
}
