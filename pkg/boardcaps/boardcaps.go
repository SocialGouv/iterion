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
