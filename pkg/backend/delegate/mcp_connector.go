package delegate

import (
	"github.com/SocialGouv/iterion/pkg/connection"
)

// The connector MCP facade's server name (#2758): the name under which
// iterion registers its internal connector facade with an agent's CLI, so the
// tools read mcp__iterion_connectors__<tool>. Registered over HTTP ONLY —
// the credential a facade call spends is resolved server-side and must never
// enter the sandbox, which is exactly what a stdio transport would defeat.
const connectorMCPServerName = "iterion_connectors"

// connectorToolFQN returns the MCP fully-qualified name a bot sees for one of
// the facade's tools.
func connectorToolFQN(tool string) string {
	return "mcp__" + connectorMCPServerName + "__" + tool
}

// HasConnectorsCapability reports whether the granted-cap list contains the
// `connectors` capability — the DSL spelling that wires a node to the
// connector MCP facade (connection.CapConnectors is the one source of the
// string).
func HasConnectorsCapability(caps []string) bool {
	for _, c := range caps {
		if c == connection.CapConnectors {
			return true
		}
	}
	return false
}

// ConnectorToolsFor returns the tool FQNs the facade adds to a node's
// allowed-tool set when that set is restricted: the discovery pair. The
// curated per-operation tools are package-dependent, so a restricted toolset
// must glob for them (mcp__iterion_connectors__*) — the pair is what the
// facade guarantees under a fixed name.
func ConnectorToolsFor(caps []string) []string {
	if !HasConnectorsCapability(caps) {
		return nil
	}
	return []string{
		connectorToolFQN("search_operations"),
		connectorToolFQN("call_operation"),
	}
}
