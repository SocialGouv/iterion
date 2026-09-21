package ir

// RunMembers is the exhaustive `run.<member>` vocabulary — the one list the
// compiler validates `{{run.*}}` references against (C153,
// validateRunRef) and the runtime's namespace resolver mirrors
// (pkg/runtime, RunNamespaceMembers). It lives beside the reference parser
// so a member cannot be added to the runtime without the compiler knowing:
// an unknown member renders EMPTY at run time (resolveRunPath returns nil),
// and a scope gate whose exclusion list silently vanished from its git
// command is the failure #1464 exists to prevent.
var RunMembers = []string{
	"id",
	"elapsed_seconds",
	"cost_usd",
	"tokens",
	"iterations",
	"max_duration_seconds",
	"max_cost_usd",
	"max_tokens",
	"max_iterations",
	"tree_noise",
}
