package model

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"sync"

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
// in-place retry of the executor, and the engine's recovery re-run of the
// node, re-enter Execute on the same conversation. Every loop is charged the
// completed tool turns the conversation it is handed holds after the
// execution's anchor — the last completed tool call of the conversation its
// first attempt started from — and is handed what is left of the bound.
//
// The charge is read from the conversation, never from a count kept beside
// it: a turn is spent only when the loop sees it again. A step the stream
// cut before it finished is in no conversation and spends nothing; a retry
// that replays none of the dead attempt's turns (no store, a sandboxed
// crossing whose capture never reached the host) gets the whole bound back,
// so what it answers stays grounded in turns it can see. The anchor is an
// identity, not a count, and a compaction keeps it: the summary it leaves
// names every completed tool turn it folded (compactMessagesToolSafe), so
// the turns after the anchor are counted whichever of them, or of the
// starting conversation's, it folded.
//
// Execute pins one per invocation on the context (withToolStepBudget),
// beside the command scope, and runToolLoop is the one door every tool loop
// of the invocation goes through. A node execution is sequential, so the
// budget takes no lock.
type toolStepBudget struct {
	// limit is the bound the loop applies: the node's tool_max_steps, or the
	// default a loop gets when the node names none.
	limit int
	// anchor names the last completed tool turn of the conversation the
	// node execution started from (lastToolTurnAnchor, "" when it held
	// none); anchored says
	// it is known — carried by the task (delegate.Task.ToolTurnsAnchor), or
	// fixed by this invocation's first loop.
	anchor   string
	anchored bool
	// ran counts this invocation's own completed tool turns — the figure
	// delegate.Result.ToolTurns reports. It charges nothing.
	ran int
	// sessKey is the session store key of the task (taskSessionKey) the
	// loops capture their completed tool rounds under.
	sessKey string
}

// newToolStepBudget returns the budget of an invocation whose loop applies
// maxSteps — GenerateTextDirect's own default when it is not positive, the
// bound such a loop runs under — and whose node execution is anchored at
// *anchor (nil: this invocation is the execution's first, and its first
// loop fixes the anchor).
func newToolStepBudget(maxSteps int, anchor *string) *toolStepBudget {
	if maxSteps <= 0 {
		maxSteps = defaultMaxSteps
	}
	b := &toolStepBudget{limit: maxSteps}
	if anchor != nil {
		b.anchor, b.anchored = *anchor, true
	}
	return b
}

// charge is what a loop handed conversation has already spent: its
// completed tool turns after the anchor. The first conversation the budget
// sees fixes the anchor when none was carried.
func (b *toolStepBudget) charge(conversation []api.Message) int {
	if !b.anchored {
		b.anchor, b.anchored = lastToolTurnAnchor(conversation), true
	}
	return completedToolRoundsAfter(conversation, b.anchor)
}

// turnsRun is this invocation's completed tool turns; zero for a nil budget.
func (b *toolStepBudget) turnsRun() int {
	if b == nil {
		return 0
	}
	return b.ran
}

// anchorReport is the anchor for delegate.Result.ToolTurnsAnchor: nil when
// the budget is nil or never fixed one.
func (b *toolStepBudget) anchorReport() *string {
	if b == nil || !b.anchored {
		return nil
	}
	anchor := b.anchor
	return &anchor
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

// completedToolRounds counts the completed tool turns a conversation holds
// (toolTurnKeys): the turns a compaction summary records as folded, and the
// visible ones — an assistant message carrying tool calls, followed by the
// user message that carries a result for one of them, the pair one loop step
// appends. A call still waiting for its result (a pause) is no completed
// turn, and the result a resumed pause is answered with completes it.
func completedToolRounds(conversation []api.Message) int {
	return len(toolTurnKeys(conversation))
}

// answeredCalls is the ids of the tool calls in calls that answers carries a
// result for — none unless calls is an assistant message and answers the
// user message after it.
func answeredCalls(calls, answers api.Message) []string {
	if calls.Role != "assistant" || answers.Role != "user" {
		return nil
	}
	var ids []string
	for _, block := range calls.Content {
		if block.Type == "tool_use" && slices.ContainsFunc(answers.Content, func(r api.ContentBlock) bool {
			return r.Type == "tool_result" && r.ToolUseID == block.ID
		}) {
			ids = append(ids, block.ID)
		}
	}
	return ids
}

// lastToolTurnAnchor anchors at the last completed tool turn of
// conversation (toolTurnKeys), "" when it holds none. The anchor names the
// turn by its key and the occurrence of that key — a provider may reuse a
// call id from one response to the next ("call_0", "functions.x:0"), and the
// anchor is the LAST occurrence the conversation holds, never the first.
func lastToolTurnAnchor(conversation []api.Message) string {
	keys := toolTurnKeys(conversation)
	if len(keys) == 0 {
		return ""
	}
	last := keys[len(keys)-1]
	occurrence := 0
	for _, key := range keys {
		if key == last {
			occurrence++
		}
	}
	return strconv.Itoa(occurrence) + ":" + last
}

// anchorIndex is the index in keys of the turn anchor names, -1 when anchor
// is "", unreadable, or names no turn of keys.
func anchorIndex(keys []string, anchor string) int {
	ordinal, key, ok := strings.Cut(anchor, ":")
	occurrence, err := strconv.Atoi(ordinal)
	if !ok || err != nil || occurrence < 1 {
		return -1
	}
	for i, k := range keys {
		if k == key {
			if occurrence--; occurrence == 0 {
				return i
			}
		}
	}
	return -1
}

// completedToolRoundsAfter counts the completed tool turns of conversation
// after the one anchor names — folded turns included, as the compaction
// summary names them; every one when anchor is "" or names no turn of the
// conversation.
func completedToolRoundsAfter(conversation []api.Message, anchor string) int {
	keys := toolTurnKeys(conversation)
	if i := anchorIndex(keys, anchor); i >= 0 {
		return len(keys) - i - 1
	}
	return len(keys)
}

// executedToolAfter reports whether conversation holds, after the turn
// anchor names, a tool result that is not an error: a tool that RAN. A
// malformed, refused or unknown call completes a turn with an error result
// and executes nothing; a tool that ran and failed reads the same and is
// not counted either, the transcript cannot tell the two apart. When anchor
// is "" or names no visible turn, every visible result is after it.
func executedToolAfter(conversation []api.Message, anchor string) bool {
	keys := toolTurnKeys(conversation)
	target := anchorIndex(keys, anchor)
	from, seen := 0, 0
	for i, message := range conversation {
		for _, block := range message.Content {
			if block.Type == "text" && strings.HasPrefix(block.Text, foldedToolTurnsMarker) {
				seen += len(strings.Fields(strings.TrimSuffix(strings.TrimPrefix(block.Text, foldedToolTurnsMarker), "]")))
			}
		}
		if i+1 < len(conversation) && len(answeredCalls(message, conversation[i+1])) > 0 {
			if seen == target {
				from = i + 2
			}
			seen++
		}
	}
	for _, message := range conversation[min(from, len(conversation)):] {
		if message.Role != "user" {
			continue
		}
		for _, block := range message.Content {
			if block.Type == "tool_result" && !block.IsError {
				return true
			}
		}
	}
	return false
}

// runToolLoop runs a tool loop of the invocation within the budget pinned on
// ctx: the loop is charged what the conversation it is handed already holds
// after the anchor, and handed the rest as its MaxSteps. ran is false when
// nothing is left: no request is made, and the caller goes on to what
// follows an exhausted loop. A loop is never handed MaxSteps 0, which
// GenerateTextDirect reads as its own default. onRun, when set, runs once
// the loop is decided to run, before its first request — where a caller
// announces it.
//
// A loop whose conversation already holds a tool this node execution ran
// is not forced to call a tool first (ForceInitialToolUse): the grounding
// that force exists for is already in view, and a model that answers from it
// would otherwise be refused or made to call a tool again. A turn answered
// with an error — a call malformed, refused or unknown — grounds nothing,
// and lifts nothing (executedToolAfter).
//
// Options without tools, or a ctx with no budget (a direct caller), run as
// given.
func runToolLoop(ctx context.Context, client api.APIClient, opts GenerationOptions, onRun func()) (result *TextResult, ran bool, err error) {
	budget := toolStepBudgetFrom(ctx)
	if budget != nil && len(opts.Tools) > 0 {
		charged := budget.charge(opts.Messages)
		left := budget.limit - charged
		if left <= 0 {
			return nil, false, nil
		}
		opts.MaxSteps = left
		if charged > 0 && executedToolAfter(opts.Messages, budget.anchor) {
			opts.ForceInitialToolUse = false
		}
		opts.OnTurnCapture = captureToolRounds(ctx, budget.sessKey, opts.OnTurnCapture)
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

// lastAssistantText is the text of the last assistant message of
// conversation, "" when it holds none or that message carries no text.
func lastAssistantText(conversation []api.Message) string {
	for i := len(conversation) - 1; i >= 0; i-- {
		if conversation[i].Role != "assistant" {
			continue
		}
		var text strings.Builder
		for _, block := range conversation[i].Content {
			if block.Type == "text" {
				text.WriteString(block.Text)
			}
		}
		return text.String()
	}
	return ""
}

// captureToolRounds chains onto a loop's turn hook the capture of its
// conversation after every completed tool round (captureSessionMessages):
// what the loop completed reaches the session store — and, from a
// sandboxed runner, the host — as it completes, so a runner that dies
// mid-loop leaves its completed turns where its retry replays them. The
// final turn is the attempt's own capture, not a round.
func captureToolRounds(ctx context.Context, sessKey string, next func(TurnCaptureInfo)) func(TurnCaptureInfo) {
	return func(turn TurnCaptureInfo) {
		if turn.Result.FinishReason == FinishToolCalls && len(turn.Result.ToolCalls) > 0 {
			captureSessionMessages(ctx, sessKey, &TextResult{Messages: turn.Conversation})
		}
		if next != nil {
			next(turn)
		}
	}
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

// carriedConversation is the conversation a task that carries its own
// history starts from, as Execute composes it, for its completed tool
// turns: a resumed pause with its pending call answered, a continued
// conversation as it ended (the user turn appended to it carries no tool
// call). Nil when the task carries none, or carries one that does not
// decode — Execute refuses that task before any loop runs.
func carriedConversation(task delegate.Task) []api.Message {
	var prior []api.Message
	switch {
	case len(task.ResumeConversation) > 0:
		if json.Unmarshal(task.ResumeConversation, &prior) != nil {
			return nil
		}
		prior = append(prior, api.Message{Role: "user", Content: []api.ContentBlock{
			api.ToolResult{ToolUseID: task.ResumePendingToolUseID}.ToContentBlock(),
		}})
	case len(task.ContinueConversation) > 0:
		if json.Unmarshal(task.ContinueConversation, &prior) != nil {
			return nil
		}
	}
	return prior
}

// crossingTurns is the launcher's account of a sandboxed crossing's tool
// turns, read from what crossed rather than from the runner's result alone:
// a runner that ends before its result envelope — killed, or exiting
// without one — has already mirrored the captures it sent to the host
// (session_capture), and the next crossing replays them.
//
// start anchors at the last completed tool turn of the conversation the
// crossing's first loop starts from (lastToolTurnAnchor), as the launcher
// hands it over: the session replay it sends, or the history the task
// carries. anchor is the node
// execution's (delegate.Task.ToolTurnsAnchor), start itself on a first
// crossing — the anchor the runner is handed, so it never fixes one from a
// replay that already holds a dead crossing's turns.
type crossingTurns struct {
	start  string
	anchor string
	// mu guards the last capture's completed turns after start, -1 before
	// the first capture.
	mu       sync.Mutex
	captured int
}

// newCrossingTurns accounts the crossing of task, handed seed.
func newCrossingTurns(task delegate.Task, seed sessionSeed) *crossingTurns {
	c := &crossingTurns{captured: -1}
	switch {
	case len(task.ResumeConversation) > 0 || len(task.ContinueConversation) > 0:
		c.start = lastToolTurnAnchor(carriedConversation(task))
	case len(seed.replay) > 0:
		var replay []api.Message
		if json.Unmarshal(seed.replay, &replay) == nil {
			c.start = lastToolTurnAnchor(replay)
		}
	}
	c.anchor = c.start
	if task.ToolTurnsAnchor != nil {
		c.anchor = *task.ToolTurnsAnchor
	}
	return c
}

// observe records the capture the runner mirrored.
func (c *crossingTurns) observe(snapshot json.RawMessage) {
	var conversation []api.Message
	if json.Unmarshal(snapshot, &conversation) != nil {
		return
	}
	turns := completedToolRoundsAfter(conversation, c.start)
	c.mu.Lock()
	c.captured = turns
	c.mu.Unlock()
}

// counted is the tool turns the crossing's captures show it completed: the
// last capture's after the start. Zero before any capture.
func (c *crossingTurns) counted() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return max(c.captured, 0)
}

// stamp writes the account onto a result the crossing returns, whatever its
// outcome: the anchor the runner was handed, and the larger of the runner's
// own count and the captures' — the envelope's when it arrived, the
// captures' alone when it did not.
func (c *crossingTurns) stamp(res *delegate.Result) {
	anchor := c.anchor
	res.ToolTurnsAnchor = &anchor
	res.ToolTurns = max(res.ToolTurns, c.counted())
}
