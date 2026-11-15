package model

import (
	"context"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
)

// toolStepBudget is the tool-step bound (`tool_max_steps`) ONE claw node
// execution shares.
//
// An execution runs its tool loop more than once: a retryLoop attempt
// re-issues the turn after a transient failure and replays the dead
// attempt's history from the session store (applySessionMessagesForTask);
// the tool-use nudge re-runs the loop on the first pass's conversation; an
// in-place retry of the executor re-enters Execute on the same conversation
// (delegate.Task.ToolTurnsSpent). Each run is handed what is left of the
// bound and spends the tool turns it completes, so the bound holds across
// all of them: handing every run the node's whole bound let a node with
// `tool_max_steps: 30` complete 26, then 2, then 30 tool turns.
//
// A completed tool turn is a model step whose tool calls ran
// (completedToolTurns). A step the stream cut before it finished spends
// nothing, so a retry still gets the turn the failure took.
//
// Execute pins one per invocation on the context (withToolStepBudget),
// beside the command scope, and runToolLoop is the one door every tool loop
// of the invocation goes through. A node execution is sequential, so the
// budget takes no lock.
type toolStepBudget struct {
	// limit is the bound the loop applies: the node's tool_max_steps, or the
	// default a loop gets when the node names none.
	limit int
	// spent counts the completed tool turns of the whole node execution:
	// the earlier invocations' (the seed) and this one's.
	spent int
	// ran counts this invocation's own completed tool turns — the figure
	// delegate.Result.ToolTurns reports.
	ran int
}

// newToolStepBudget returns the budget of an invocation whose loop applies
// maxSteps — GenerateTextDirect's own default when it is not positive, the
// bound such a loop runs under — and whose node execution already completed
// spent tool turns.
func newToolStepBudget(maxSteps, spent int) *toolStepBudget {
	if maxSteps <= 0 {
		maxSteps = defaultMaxSteps
	}
	return &toolStepBudget{limit: maxSteps, spent: max(spent, 0)}
}

// left is what the next loop may still run; zero or less means nothing.
func (b *toolStepBudget) left() int { return b.limit - b.spent }

// exhausted reports a budget with nothing left. A nil budget (no tool loop,
// or a caller that pinned none) is never exhausted.
func (b *toolStepBudget) exhausted() bool { return b != nil && b.left() <= 0 }

// spend records the tool turns a loop completed.
func (b *toolStepBudget) spend(turns int) {
	b.spent += turns
	b.ran += turns
}

// turnsRun is this invocation's completed tool turns; zero for a nil budget.
func (b *toolStepBudget) turnsRun() int {
	if b == nil {
		return 0
	}
	return b.ran
}

type toolStepBudgetKey struct{}

// withToolStepBudget pins the invocation's budget on ctx.
func withToolStepBudget(ctx context.Context, b *toolStepBudget) context.Context {
	return context.WithValue(ctx, toolStepBudgetKey{}, b)
}

// toolStepBudgetFrom returns the budget pinned on ctx, nil when none is.
func toolStepBudgetFrom(ctx context.Context) *toolStepBudget {
	b, _ := ctx.Value(toolStepBudgetKey{}).(*toolStepBudget)
	return b
}

// completedToolTurns counts the steps of a loop's result — a partial one
// included, as GenerateTextDirect returns it beside a failure — whose tool
// calls ran: a step that ended on tool calls is the one the loop executes
// tools for (GenerateTextDirect stops on any other).
func completedToolTurns(r *TextResult) int {
	if r == nil {
		return 0
	}
	n := 0
	for i := range r.Steps {
		if r.Steps[i].FinishReason == FinishToolCalls && len(r.Steps[i].ToolCalls) > 0 {
			n++
		}
	}
	return n
}

// runToolLoop runs a tool loop of the invocation within the budget pinned on
// ctx: the loop is handed what is left as its MaxSteps, and the tool turns it
// completes — a failed loop's partial ones too — are spent before it returns.
// ran is false when nothing was left: no request is made, and the caller goes
// on to what follows an exhausted loop. A loop is never handed MaxSteps 0,
// which GenerateTextDirect reads as its own default.
//
// Options without tools, or a ctx with no budget (a direct caller), run as
// given.
func runToolLoop(ctx context.Context, client api.APIClient, opts GenerationOptions) (result *TextResult, ran bool, err error) {
	budget := toolStepBudgetFrom(ctx)
	if budget == nil || len(opts.Tools) == 0 {
		result, err = GenerateTextDirect(ctx, client, opts)
		return result, true, err
	}
	left := budget.left()
	if left <= 0 {
		return nil, false, nil
	}
	opts.MaxSteps = left
	result, err = GenerateTextDirect(ctx, client, opts)
	budget.spend(completedToolTurns(result))
	return result, true, err
}
