package delegate

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
)

// boardMCPServerName is the name under which iterion registers its internal
// board MCP server with the claude_code CLI subprocess. The MCP convention
// gives every tool the FQN mcp__<serverName>__<toolName>.
const boardMCPServerName = "iterion_board"

// boardMCPSubcommand is the cobra subcommand exposed by the iterion binary
// when invoked as `iterion __mcp-board`. See cmd/iterion/mcp_board.go.
const boardMCPSubcommand = "__mcp-board"

// boardCapabilityPrefix is the namespace prefix that flags a capability as
// addressing the board. Capabilities outside this prefix are ignored by the
// board wiring (they may be honoured by other future subsystems).
const boardCapabilityPrefix = "board."

// boardToolFQN returns the MCP fully-qualified name a bot sees for the given
// bare tool name.
func boardToolFQN(tool string) string {
	return "mcp__" + boardMCPServerName + "__" + tool
}

// HasBoardCapability reports whether the granted-cap list contains any
// `board.*` entry — used to decide whether to register the board MCP server
// at all.
func HasBoardCapability(caps []string) bool {
	return hasCapabilityPrefix(caps, boardCapabilityPrefix)
}

// BoardToolsFor returns the MCP FQN list the granted capabilities unlock,
// derived from boardops.ToolsFor so the tool→capability mapping has a
// single source of truth. Order matches boardops.ToolsFor (sorted by name).
func BoardToolsFor(caps []string) []string {
	c := boardops.Capabilities{}
	for _, name := range caps {
		c[name] = true
	}
	tools := boardops.ToolsFor(c)
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, boardToolFQN(t.Name))
	}
	return out
}

// IsIterionMCPTool reports whether a tool FQN is served by one of iterion's
// OWN MCP transports — the board and runs servers this package registers for a
// node's `capabilities:` — as opposed to a tool the node's `mcp:` blocks or the
// harness brought.
//
// It is built from the same two server-name constants the FQNs are built from,
// so it cannot drift into recognising a spelling nothing emits. Its consumer is
// the workspace-safety classifier: these tools act on the BOARD and on the run
// store, never on the shared worktree, which is the only thing that classifier
// asks about. (Moving a card to `eligible` does start a run through the
// dispatcher — a real effect, and not a write to the workspace this node
// shares with its parallel siblings.)
func IsIterionMCPTool(name string) bool {
	return strings.HasPrefix(name, "mcp__"+boardMCPServerName+"__") ||
		strings.HasPrefix(name, "mcp__"+runsMCPServerName+"__")
}
