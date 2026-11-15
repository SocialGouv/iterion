package model

import (
	"context"
	"encoding/json"
	"slices"
	"sync/atomic"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// toolStepBudget is the tool-step bound (`tool_max_steps`) ONE claw node
// execution shares.
//
// An execution runs its tool loop more than once: a retryLoop attempt
// re-issues the turn after a transient failure and replays the dead
// attempt's history from the session store (applySessionMessagesForTask);
// the tool-use nudge re-runs the loop on the first pass's conversation; an
// in-place retry of the executor re-enters Execute on the same conversation.
// Every loop is charged the completed tool turns the conversation it is
// handed already holds beyond the execution's seed — the conversation its
// first attempt started from — and is handed what is left of the bound.
//
// The charge is read from the conversation, never from a count kept beside
// it: a turn is spent only when the loop sees it again. A step the stream
// cut before it finished is in no conversation and spends nothing; a retry
// that replays none of the dead attempt's turns (no store, a sandboxed
// crossing whose capture never reached the host) gets the whole bound back,
// so what it answers stays grounded in turns it can see.
//
// Execute pins one per invocation on the context (withToolStepBudget),
// beside the command scope, and runToolLoop is the one door every tool loop
// of the invocation goes through. A node execution is sequential, so the
// budget takes no lock.
type toolStepBudget struct {
	// limit is the bound the loop applies: the node's tool_max_steps, or the
	// default a loop gets when the node names none.
	limit int
	// seed is the completed tool turns of the conversation the node
	// execution started from; seeded says it is known — carried by the task
	// (delegate.Task.ToolTurnsSeed), or fixed by this invocation's first
	// loop.
	seed   int
	seeded bool
	// ran counts this invocation's own completed tool turns — the figure
	// delegate.Result.ToolTurns reports. It charges nothing.
	ran int
}

// newToolStepBudget returns the budget of an invocation whose loop applies
// maxSteps — GenerateTextDirect's own default when it is not positive, the
// bound such a loop runs under — and whose node execution started from a
// conversation holding *seed completed tool turns (nil: this invocation is
// the execution's first, and its first loop fixes the seed).
func newToolStepBudget(maxSteps int, seed *int) *toolStepBudget {
	if maxSteps <= 0 {
		maxSteps = defaultMaxSteps
	}
	b := &toolStepBudget{limit: maxSteps}
	if seed != nil {
		b.seed, b.seeded = max(*seed, 0), true
	}
	return b
}

// charge is what a loop handed conversation has already spent: its
// completed tool turns beyond the seed. The first conversation the budget
// sees fixes the seed when none was carried.
func (b *toolStepBudget) charge(conversation []api.Message) int {
	turns := completedToolRounds(conversation)
	if !b.seeded {
		b.seed, b.seeded = turns, true
	}
	return max(turns-b.seed, 0)
}

// turnsRun is this invocation's completed tool turns; zero for a nil budget.
func (b *toolStepBudget) turnsRun() int {
	if b == nil {
		return 0
	}
	return b.ran
}

// seedReport is the seed for delegate.Result.ToolTurnsSeed: nil when the
// budget is nil or never fixed one.
func (b *toolStepBudget) seedReport() *int {
	if b == nil || !b.seeded {
		return nil
	}
	seed := b.seed
	return &seed
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

// completedToolRounds counts the completed tool turns a conversation holds:
// an assistant message carrying tool calls, followed by the user message
// that carries a result for one of them — the pair one loop step appends. A
// call still waiting for its result (a pause) is no completed turn, and the
// result a resumed pause is answered with completes it.
func completedToolRounds(conversation []api.Message) int {
	n := 0
	for i := 0; i+1 < len(conversation); i++ {
		calls, answers := conversation[i], conversation[i+1]
		if calls.Role != "assistant" || answers.Role != "user" {
			continue
		}
		var ids []string
		for _, block := range calls.Content {
			if block.Type == "tool_use" {
				ids = append(ids, block.ID)
			}
		}
		if len(ids) > 0 && slices.ContainsFunc(answers.Content, func(block api.ContentBlock) bool {
			return block.Type == "tool_result" && slices.Contains(ids, block.ToolUseID)
		}) {
			n++
		}
	}
	return n
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
// ctx: the loop is charged what the conversation it is handed already holds
// beyond the seed, and handed the rest as its MaxSteps. ran is false when
// nothing is left: no request is made, and the caller goes on to what
// follows an exhausted loop. A loop is never handed MaxSteps 0, which
// GenerateTextDirect reads as its own default. onRun, when set, runs once
// the loop is decided to run, before its first request — where a caller
// announces it.
//
// Options without tools, or a ctx with no budget (a direct caller), run as
// given.
func runToolLoop(ctx context.Context, client api.APIClient, opts GenerationOptions, onRun func()) (result *TextResult, ran bool, err error) {
	budget := toolStepBudgetFrom(ctx)
	if budget != nil && len(opts.Tools) > 0 {
		left := budget.limit - budget.charge(opts.Messages)
		if left <= 0 {
			return nil, false, nil
		}
		opts.MaxSteps = left
	}
	if onRun != nil {
		onRun()
	}
	result, err = GenerateTextDirect(ctx, client, opts)
	if budget != nil && len(opts.Tools) > 0 {
		budget.ran += completedToolTurns(result)
	}
	return result, true, err
}

// carriedToolRounds is the completed tool turns of the conversation a task
// that carries its own history starts from, as Execute composes it: a
// resumed pause with its pending call answered, a continued conversation as
// it ended (the user turn appended to it carries no tool call). Zero when
// the task carries none, or carries one that does not decode — Execute
// refuses that task before any loop runs.
func carriedToolRounds(task delegate.Task) int {
	var prior []api.Message
	switch {
	case len(task.ResumeConversation) > 0:
		if json.Unmarshal(task.ResumeConversation, &prior) != nil {
			return 0
		}
		prior = append(prior, api.Message{Role: "user", Content: []api.ContentBlock{
			api.ToolResult{ToolUseID: task.ResumePendingToolUseID}.ToContentBlock(),
		}})
	case len(task.ContinueConversation) > 0:
		if json.Unmarshal(task.ContinueConversation, &prior) != nil {
			return 0
		}
	}
	return completedToolRounds(prior)
}

// crossingTurns is the launcher's account of a sandboxed crossing's tool
// turns, read from what crossed rather than from the runner's result alone:
// a runner that ends before its result envelope — killed, or exiting
// without one — has already mirrored the captures it sent to the host
// (session_capture), and the next crossing replays them.
//
// start is the completed tool turns of the conversation the crossing's first
// loop starts from, as the launcher hands it over: the session replay it
// sends, or the history the task carries. seed is the node execution's
// (delegate.Task.ToolTurnsSeed), the start itself on a first crossing — the
// seed the runner is handed, so it never fixes one from a replay that
// already holds a dead crossing's turns.
type crossingTurns struct {
	start int
	seed  int
	// captured is the completed tool turns of the last capture the runner
	// mirrored, -1 before the first.
	captured atomic.Int64
}

// newCrossingTurns accounts the crossing of task, handed seed.
func newCrossingTurns(task delegate.Task, seed sessionSeed) *crossingTurns {
	c := &crossingTurns{}
	switch {
	case len(task.ResumeConversation) > 0 || len(task.ContinueConversation) > 0:
		c.start = carriedToolRounds(task)
	case len(seed.replay) > 0:
		var replay []api.Message
		if json.Unmarshal(seed.replay, &replay) == nil {
			c.start = completedToolRounds(replay)
		}
	}
	c.seed = c.start
	if task.ToolTurnsSeed != nil {
		c.seed = *task.ToolTurnsSeed
	}
	c.captured.Store(-1)
	return c
}

// observe records the capture the runner mirrored.
func (c *crossingTurns) observe(snapshot json.RawMessage) {
	var conversation []api.Message
	if json.Unmarshal(snapshot, &conversation) == nil {
		c.captured.Store(int64(completedToolRounds(conversation)))
	}
}

// counted is the tool turns the crossing's captures show it completed: the
// last capture's beyond the start. Zero before any capture.
func (c *crossingTurns) counted() int {
	return max(int(c.captured.Load())-c.start, 0)
}

// stamp writes the account onto a result the crossing returns, whatever its
// outcome: the seed the runner was handed, and the larger of the runner's
// own count and the captures' — the envelope's when it arrived, the
// captures' alone when it did not.
func (c *crossingTurns) stamp(res *delegate.Result) {
	seed := c.seed
	res.ToolTurnsSeed = &seed
	res.ToolTurns = max(res.ToolTurns, c.counted())
}
