// Package boardcaps is the vocabulary of the native kanban board's
// capability names: the ONE list the DSL compiler validates a
// `capabilities:` entry against and the board implementation
// (pkg/dispatcher/native/boardops) gates each operation on.
//
// It is a leaf by design, and that is the whole point of the package:
// pkg/dsl/ir validates against these names, and reading seven strings must
// not link the native dispatcher, the store, the forge and the mailer into
// every binary and every test that compiles a .bot — the packaging defect
// filed as iterion#2857, where the vocabulary sat inside the implementation
// (boardops) and the IR paid its 43 module packages for seven constants.
package boardcaps

// The board capability names. A name added here is a capability the
// implementation must gate on: boardops re-exports them (so its existing
// callers read the same constants) and pins every one to at least one tool
// of its registry.
const (
	BoardRead    = "board.read"
	BoardCreate  = "board.create"
	BoardMove    = "board.move"
	BoardAssign  = "board.assign"
	BoardLabel   = "board.label"
	BoardClose   = "board.close"
	BoardComment = "board.comment"
)

// ConnectorID is the connector id the board's NATIVE action transport owns
// (docs/adr/151, iterion#2834): the first segment of every `board.*` action
// id a tool node writes, e.g. `board.issue.list`. It lives in this leaf for
// the same reason the capability names do: the DSL compiler reads it to
// know a `board.*` action needs no `connection:` (a native connector holds
// no credential), and reading one string must not link the dispatcher into
// every binary that compiles a .bot.
const ConnectorID = "board"

// NativeConnectorIDs is the set of connector ids a NATIVE action transport
// owns (ADR-151): an in-process, credential-free connector the engine
// executes itself rather than reaching over HTTP. Two readers, neither of
// which may link the dispatcher: the DSL compiler (`connection:` optional
// for these alone) and the connector-package validator (the id is RESERVED —
// a catalog package squatting it would be hijacked where the transport is
// registered and refused where it is not). A new native connector adds its
// id HERE as part of its seam registration (G110: registration IS the seam),
// never an `if` at a call site.
var NativeConnectorIDs = map[string]bool{
	ConnectorID: true,
}

// ActionCapability maps every action id the native board facade exposes to
// the capability gating it — the compile-time half of the facade's grant:
// the DSL compiler (C334) warns when a native board action's capability is
// declared nowhere (neither the node nor the workflow) and C336 when an
// action names a verb absent from it, and the runtime refusals they predict
// are boardops' own caps.Has and the executor's admission. boardops' matrix
// test holds this table to the facade's bindings and to the native registry,
// in both directions.
var ActionCapability = map[string]string{
	"board.issue.get":        BoardRead,
	"board.issue.list":       BoardRead,
	"board.issue.create":     BoardCreate,
	"board.issue.label":      BoardLabel,
	"board.issue.transition": BoardMove,
}

// ToolCapability maps every bare tool name of the board MCP family
// (`mcp.iterion_board.*`) to the capability gating it — the compile-time
// half of the runtime's refusal: the DSL compiler (C328) warns when a node
// names one of these without holding the capability, and naming the tool is
// what the board's own `capabilities:` documentation cannot do for the
// author. boardops.allTools must carry exactly these names and pins — the
// conformance test in that package holds the two tables to each other.
var ToolCapability = map[string]string{
	"add_labels":       BoardLabel,
	"assign_issue":     BoardAssign,
	"close_issue":      BoardClose,
	"comment_issue":    BoardComment,
	"create_issue":     BoardCreate,
	"get_issue":        BoardRead,
	"list_issues":      BoardRead,
	"list_labels":      BoardRead,
	"remove_labels":    BoardLabel,
	"set_bot":          BoardAssign,
	"set_labels":       BoardLabel,
	"transition_issue": BoardMove,
}
