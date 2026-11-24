package scripthost

import "context"

// The script result budget rides the context of a NESTED tool call.
//
// [Limits.ToolResultBytes] is the bound one tool result handed back to a
// script must respect. The parent clips what crosses the tool channel itself,
// but a tool whose OWN output cap is smaller than that bound caps the script
// for no reason: the bash tool keeps 10 KiB of a command's output for a
// direct call, and a script whose result budget is 64 KiB would never see
// more than 10 KiB of a build log. The budget is therefore pinned on the
// dispatch context so such a tool can re-cap itself to what the script may
// actually receive.
//
// The key lives HERE rather than beside the other context pins because of the
// dependency direction: the reader is a claw tool, and the package that owns
// the node's context pins imports claw — a second accessor there would close
// a cycle. The budget is a [Limits] concept, and both sides already import
// this package, so this is the one home that owns the number and serves both.
//
// It is armed per call, on the per-call context [scriptToolServer.CallTool]
// derives: two calls of one batch carry the same number, but each derives its
// own context, so arming is safe under [MaxParallelBatch] workers without a
// shared mutable.
type ctxKeyScriptResultBudget struct{}

// WithScriptResultBudget pins the calling script's per-result byte budget on
// the context a nested tool's Execute receives. budget must be positive;
// arming a non-positive budget is a caller bug and the reader refuses it, so
// the zero value never means "unbounded".
func WithScriptResultBudget(ctx context.Context, budget int64) context.Context {
	return context.WithValue(ctx, ctxKeyScriptResultBudget{}, budget)
}

// ScriptResultBudgetFrom reads the pinned budget. The second return is false
// when the call does not run inside a script — the direct path, where a tool
// keeps its own default cap — or when what was pinned is not a positive
// budget, which can only be a writer that broke the contract above.
func ScriptResultBudgetFrom(ctx context.Context) (int64, bool) {
	budget, ok := ctx.Value(ctxKeyScriptResultBudget{}).(int64)
	return budget, ok && budget > 0
}
