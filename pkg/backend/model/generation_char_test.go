package model

// Characterization tests for GenerateTextDirect ahead of its decomposition
// (backlog B2). They pin CURRENT observable behavior — including quirks — so
// a later extract-function refactor that changes behavior fails here, not in
// production. Seams: the scripted mockAPIClient + textEvents/toolUseEvents
// builders from generation_test.go (same package).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/claw-code-go/pkg/api/hooks"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// charNoopTool returns a trivially succeeding tool and a pointer to its
// execution counter.
func charNoopTool(name string) (GenerationTool, *int) {
	calls := new(int)
	return GenerationTool{
		Name:        name,
		Description: "test tool",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
			*calls++
			return "ok", nil
		},
	}, calls
}

// TestGenerateTextDirect_DoesNotMutateCallerMessages pins the defensive copy
// at the top of the tool loop: the caller's Messages slice (length AND
// backing array) must be untouched after a multi-step run, and the returned
// Messages carries the tool round but — quirk — NOT the final assistant text
// (the loop breaks before appending it; only the OnTurnCapture snapshot gets
// the synthesized final message).
func TestGenerateTextDirect_DoesNotMutateCallerMessages(t *testing.T) {
	client := newMockClient(
		toolUseEvents("tu_1", "noop", `{}`, 10, 5),
		textEvents("done", 10, 5),
	)
	noop, _ := charNoopTool("noop")

	// Extra capacity: if the loop appended into the caller's backing array
	// instead of a private copy, slot 1 would materialize.
	caller := make([]api.Message, 1, 8)
	caller[0] = api.Message{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}

	res, err := GenerateTextDirect(context.Background(), client, GenerationOptions{
		Model:    "claude-sonnet-4-6",
		Tools:    []GenerationTool{noop},
		Messages: caller,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(caller) != 1 {
		t.Fatalf("caller slice length mutated: %d, want 1", len(caller))
	}
	if got := caller[0].Content[0].Text; got != "hi" {
		t.Errorf("caller message mutated: %q", got)
	}
	probe := caller[:2]
	if probe[1].Role != "" || len(probe[1].Content) != 0 {
		t.Errorf("tool loop wrote into the caller's backing array: %+v", probe[1])
	}

	// Result Messages: user + assistant(tool_use) + user(tool_result); the
	// final "done" assistant text is NOT part of it.
	if len(res.Messages) != 3 {
		t.Fatalf("res.Messages = %d messages, want 3 (final assistant text excluded)", len(res.Messages))
	}
	last := res.Messages[2]
	if last.Role != "user" || len(last.Content) != 1 || last.Content[0].Type != "tool_result" {
		t.Errorf("res.Messages[2] = %+v, want the tool_result user turn", last)
	}
	if res.Text != "done" {
		t.Errorf("Text = %q, want %q", res.Text, "done")
	}
}

// TestGenerateTextDirect_PartialResultOnMidLoopError pins the partial()
// contract: when a mid-loop model call fails, the function returns BOTH a
// non-nil best-effort result (steps so far, accumulated usage, the message
// history the failed call would have seen) AND the error.
func TestGenerateTextDirect_PartialResultOnMidLoopError(t *testing.T) {
	// One script only: the SECOND StreamResponse call fails.
	client := newMockClient(toolUseEvents("tu_1", "noop", `{}`, 100, 30))
	noop, execCalls := charNoopTool("noop")

	res, err := GenerateTextDirect(context.Background(), client, GenerationOptions{
		Model:    "claude-sonnet-4-6",
		Tools:    []GenerationTool{noop},
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "go"}}}},
	})
	if err == nil {
		t.Fatal("expected the second call's failure to surface")
	}
	if res == nil {
		t.Fatal("mid-loop failure must return the best-effort partial result, not nil")
	}
	if *execCalls != 1 {
		t.Errorf("tool executed %d times, want 1", *execCalls)
	}
	if len(res.Steps) != 1 {
		t.Fatalf("partial Steps = %d, want 1", len(res.Steps))
	}
	if res.FinishReason != FinishToolCalls {
		t.Errorf("partial FinishReason = %q, want %q (last completed step)", res.FinishReason, FinishToolCalls)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "noop" {
		t.Errorf("partial ToolCalls = %+v, want the step-1 call", res.ToolCalls)
	}
	if res.TotalUsage.InputTokens != 100 || res.TotalUsage.OutputTokens != 30 {
		t.Errorf("partial TotalUsage = %+v, want step-1 usage only", res.TotalUsage)
	}
	// The conversation captured for compaction-aware retries: user +
	// assistant(tool_use) + user(tool_result).
	if len(res.Messages) != 3 {
		t.Errorf("partial Messages = %d, want 3", len(res.Messages))
	}
}

// TestGenerateTextDirect_AskUserPauseCapturesConversation pins the pause
// path: a tool returning *delegate.ErrAskUser aborts the loop with the ask
// error carrying (a) the pending tool_use ID, (b) the marshalled
// conversation INCLUDING the assistant tool_use turn but NOT a tool_result
// for it, and (c) the original question. PostToolUse/PostToolUseFailure do
// not fire for the suspension; Stop still fires (deferred).
func TestGenerateTextDirect_AskUserPauseCapturesConversation(t *testing.T) {
	client := newMockClient(toolUseEvents("tu_1", "ask_user", `{"question":"which env?"}`, 10, 5))
	askTool := GenerationTool{
		Name:        "ask_user",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "", &delegate.ErrAskUser{Question: "which env?"}
		},
	}

	r := hooks.NewRunner()
	var pre, post, postFail, stop int
	r.Register(hooks.PreToolUse, func(_ context.Context, _ hooks.Context) (hooks.Decision, error) {
		pre++
		return hooks.Decision{Action: hooks.ActionContinue}, nil
	})
	r.Register(hooks.PostToolUse, func(_ context.Context, _ hooks.Context) (hooks.Decision, error) {
		post++
		return hooks.Decision{Action: hooks.ActionContinue}, nil
	})
	r.Register(hooks.PostToolUseFailure, func(_ context.Context, _ hooks.Context) (hooks.Decision, error) {
		postFail++
		return hooks.Decision{Action: hooks.ActionContinue}, nil
	})
	r.Register(hooks.Stop, func(_ context.Context, _ hooks.Context) (hooks.Decision, error) {
		stop++
		return hooks.Decision{Action: hooks.ActionContinue}, nil
	})

	res, err := GenerateTextDirect(context.Background(), client, GenerationOptions{
		Model:    "claude-sonnet-4-6",
		Tools:    []GenerationTool{askTool},
		Hooks:    r,
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "deploy"}}}},
	})
	var askErr *delegate.ErrAskUser
	if !errors.As(err, &askErr) {
		t.Fatalf("expected *delegate.ErrAskUser, got %v", err)
	}
	if askErr.Question != "which env?" {
		t.Errorf("Question = %q", askErr.Question)
	}
	if askErr.PendingToolUseID != "tu_1" {
		t.Errorf("PendingToolUseID = %q, want tu_1", askErr.PendingToolUseID)
	}
	if len(askErr.Conversation) == 0 {
		t.Fatal("askErr.Conversation must carry the marshalled history")
	}
	var conv []api.Message
	if uErr := json.Unmarshal(askErr.Conversation, &conv); uErr != nil {
		t.Fatalf("Conversation is not valid []api.Message JSON: %v", uErr)
	}
	if len(conv) != 2 {
		t.Fatalf("Conversation = %d messages, want 2 (user + assistant tool_use, NO tool_result)", len(conv))
	}
	lastMsg := conv[1]
	if lastMsg.Role != "assistant" || len(lastMsg.Content) != 1 ||
		lastMsg.Content[0].Type != "tool_use" || lastMsg.Content[0].ID != "tu_1" {
		t.Errorf("Conversation[1] = %+v, want the pending assistant tool_use tu_1", lastMsg)
	}
	if res == nil {
		t.Fatal("pause must still return the partial result")
	}
	if pre != 1 || post != 0 || postFail != 0 || stop != 1 {
		t.Errorf("hook fires (pre,post,postFail,stop) = (%d,%d,%d,%d), want (1,0,0,1)", pre, post, postFail, stop)
	}
}

// TestGenerateTextDirect_ToolUseBlocksWithNonToolUseStopReason pins a loop
// quirk: when the response carries tool_use blocks but the stop reason is
// NOT tool_use (e.g. end_turn), the loop exits WITHOUT executing the tools —
// the calls are still reported on the result.
func TestGenerateTextDirect_ToolUseBlocksWithNonToolUseStopReason(t *testing.T) {
	events := []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: 10},
		{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "tool_use", Index: 0, ID: "tu_1", Name: "noop"}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "input_json_delta", PartialJSON: `{}`}},
		{Type: api.EventContentBlockStop, Index: 0},
		{Type: api.EventMessageDelta, StopReason: "end_turn", Usage: api.UsageDelta{OutputTokens: 5}},
		{Type: api.EventMessageStop},
	}
	client := newMockClient(events)
	noop, execCalls := charNoopTool("noop")

	res, err := GenerateTextDirect(context.Background(), client, GenerationOptions{
		Model:    "claude-sonnet-4-6",
		Tools:    []GenerationTool{noop},
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *execCalls != 0 {
		t.Errorf("tool executed %d times, want 0 (stop reason was end_turn)", *execCalls)
	}
	if got := len(client.getCalls()); got != 1 {
		t.Errorf("model calls = %d, want 1 (loop must exit)", got)
	}
	if res.FinishReason != FinishStop {
		t.Errorf("FinishReason = %q, want %q", res.FinishReason, FinishStop)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].ID != "tu_1" {
		t.Errorf("ToolCalls = %+v — the unexecuted call must still be reported", res.ToolCalls)
	}
}

// TestGenerateTextDirect_ToolUseStopReasonWithoutBlocks pins the mirror
// quirk: stop reason tool_use with ZERO tool_use blocks also exits the loop
// (no infinite spin), reporting FinishToolCalls with an empty ToolCalls.
func TestGenerateTextDirect_ToolUseStopReasonWithoutBlocks(t *testing.T) {
	events := []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: 10},
		{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "text", Index: 0}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "text_delta", Text: "hmm"}},
		{Type: api.EventContentBlockStop, Index: 0},
		{Type: api.EventMessageDelta, StopReason: "tool_use", Usage: api.UsageDelta{OutputTokens: 5}},
		{Type: api.EventMessageStop},
	}
	client := newMockClient(events)

	res, err := GenerateTextDirect(context.Background(), client, GenerationOptions{
		Model:    "claude-sonnet-4-6",
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := len(client.getCalls()); got != 1 {
		t.Errorf("model calls = %d, want 1", got)
	}
	if res.FinishReason != FinishToolCalls {
		t.Errorf("FinishReason = %q, want %q (mapped verbatim from stop reason)", res.FinishReason, FinishToolCalls)
	}
	if len(res.ToolCalls) != 0 {
		t.Errorf("ToolCalls = %+v, want none", res.ToolCalls)
	}
	if res.Text != "hmm" {
		t.Errorf("Text = %q", res.Text)
	}
}

// TestGenerateTextDirect_TurnCaptureSnapshots pins the OnTurnCapture
// contract across a 2-step run: one capture per step; the tool-round capture
// carries [user, assistant(tool_use), user(tool_result)]; the final capture
// synthesizes the assistant text message that the live history never gets.
func TestGenerateTextDirect_TurnCaptureSnapshots(t *testing.T) {
	client := newMockClient(
		toolUseEvents("tu_1", "noop", `{}`, 10, 5),
		textEvents("done", 10, 5),
	)
	noop, _ := charNoopTool("noop")

	var captures []TurnCaptureInfo
	_, err := GenerateTextDirect(context.Background(), client, GenerationOptions{
		Model:    "claude-sonnet-4-6",
		Tools:    []GenerationTool{noop},
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "go"}}}},
		OnTurnCapture: func(info TurnCaptureInfo) {
			captures = append(captures, info)
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(captures) != 2 {
		t.Fatalf("captures = %d, want 2 (one per step)", len(captures))
	}

	c0 := captures[0]
	if c0.Step != 1 || c0.Result.Number != 1 {
		t.Errorf("captures[0] step numbering = (%d,%d), want (1,1)", c0.Step, c0.Result.Number)
	}
	if len(c0.Conversation) != 3 {
		t.Fatalf("captures[0].Conversation = %d messages, want 3", len(c0.Conversation))
	}
	if c0.Conversation[2].Role != "user" || c0.Conversation[2].Content[0].Type != "tool_result" {
		t.Errorf("captures[0] last message = %+v, want the tool_result turn", c0.Conversation[2])
	}

	c1 := captures[1]
	if c1.Step != 2 || c1.Result.Number != 2 {
		t.Errorf("captures[1] step numbering = (%d,%d), want (2,2)", c1.Step, c1.Result.Number)
	}
	if len(c1.Conversation) != 4 {
		t.Fatalf("captures[1].Conversation = %d messages, want 4 (synthetic final assistant appended)", len(c1.Conversation))
	}
	final := c1.Conversation[3]
	if final.Role != "assistant" || len(final.Content) != 1 || final.Content[0].Type != "text" || final.Content[0].Text != "done" {
		t.Errorf("captures[1] final message = %+v, want synthetic assistant text %q", final, "done")
	}
}

// TestGenerateTextDirect_TurnCaptureFinalStepEmptyText pins the guard on the
// final-step synthesis: an empty assistant text appends NO synthetic message
// to the snapshot.
func TestGenerateTextDirect_TurnCaptureFinalStepEmptyText(t *testing.T) {
	events := []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: 5},
		{Type: api.EventMessageDelta, StopReason: "end_turn", Usage: api.UsageDelta{OutputTokens: 1}},
		{Type: api.EventMessageStop},
	}
	client := newMockClient(events)

	var captures []TurnCaptureInfo
	_, err := GenerateTextDirect(context.Background(), client, GenerationOptions{
		Model:    "claude-sonnet-4-6",
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
		OnTurnCapture: func(info TurnCaptureInfo) {
			captures = append(captures, info)
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(captures) != 1 {
		t.Fatalf("captures = %d, want 1", len(captures))
	}
	if len(captures[0].Conversation) != 1 {
		t.Errorf("Conversation = %d messages, want 1 (no synthetic assistant for empty text)", len(captures[0].Conversation))
	}
}

// charStubInbox is a scripted InboxHook recording call order.
type charStubInbox struct {
	mu     sync.Mutex
	order  []string
	queued []string
	drains int
}

func (s *charStubInbox) Consume(context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append(s.order, "consume")
}

func (s *charStubInbox) Drain(context.Context) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append(s.order, "drain")
	s.drains++
	if s.drains == 1 {
		return s.queued
	}
	return nil
}

// TestGenerateTextDirect_InboxDrainedBetweenIterations pins the operator
// chatbox plumbing: after a tool round, Consume runs before Drain, and the
// drained texts land as ONE synthetic user turn (system-reminder framed) at
// the tail of the next request. A 2-step run consults the inbox twice:
// once between iterations, once at the final-drain (the end-of-turn check
// added by ADR-081 so a late answer forces one more turn instead of being
// lost); the final drain here returns nothing, so the run still ends at
// step 2.
func TestGenerateTextDirect_InboxDrainedBetweenIterations(t *testing.T) {
	client := newMockClient(
		toolUseEvents("tu_1", "noop", `{}`, 10, 5),
		textEvents("done", 10, 5),
	)
	noop, _ := charNoopTool("noop")
	inbox := &charStubInbox{queued: []string{"focus on pkg/runtime", "skip the docs"}}

	_, err := GenerateTextDirect(context.Background(), client, GenerationOptions{
		Model:    "claude-sonnet-4-6",
		Tools:    []GenerationTool{noop},
		Inbox:    inbox,
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "go"}}}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := strings.Join(inbox.order, ","); got != "consume,drain,consume,drain" {
		t.Errorf("inbox call order = %q, want %q (between-iterations + final-drain, consume first each time)", got, "consume,drain,consume,drain")
	}

	calls := client.getCalls()
	if len(calls) != 2 {
		t.Fatalf("model calls = %d, want 2", len(calls))
	}
	msgs := calls[1].Messages
	if len(msgs) != 4 {
		t.Fatalf("second call messages = %d, want 4 (user, assistant, tool_result, operator turn)", len(msgs))
	}
	op := msgs[3]
	if op.Role != "user" || len(op.Content) != 1 {
		t.Fatalf("operator turn = %+v", op)
	}
	text := op.Content[0].Text
	if !strings.Contains(text, "<system-reminder>") {
		t.Errorf("operator turn missing system-reminder envelope: %q", text)
	}
	if !strings.Contains(text, "focus on pkg/runtime") || !strings.Contains(text, "skip the docs") {
		t.Errorf("operator turn missing queued texts: %q", text)
	}
	if !strings.Contains(text, "\n---\n") {
		t.Errorf("multiple operator texts must be separated by ---: %q", text)
	}
}

// TestGenerateTextDirect_CompactionReseedsTodoOnlyWithTodoTool pins the
// post-compaction reseed: when the tool set includes todo_write, each
// compaction appends the todo-reseed user turn; without todo_write, no
// reseed message ever appears.
func TestGenerateTextDirect_CompactionReseedsTodoOnlyWithTodoTool(t *testing.T) {
	const reseedMarker = "conversation history was just compacted"
	bigOutput := strings.Repeat("filler word ", 1000) // ~12 KB per tool round

	run := func(t *testing.T, toolName string) (calls []api.CreateMessageRequest, compactions int) {
		t.Helper()
		scripts := make([][]api.StreamEvent, 6)
		for i := range scripts {
			scripts[i] = toolUseEvents(fmt.Sprintf("tu_%d", i), toolName, `{}`, 10, 5)
		}
		client := newMockClient(scripts...)
		tool := GenerationTool{
			Name:        toolName,
			InputSchema: json.RawMessage(`{"type":"object"}`),
			Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
				return bigOutput, nil
			},
		}
		_, err := GenerateTextDirect(context.Background(), client, GenerationOptions{
			// Unknown model → legacy 10k-token fallback threshold, so the
			// ~12 KB tool results trigger compaction within a few rounds.
			Model:     "test-only-unknown-model",
			Tools:     []GenerationTool{tool},
			MaxSteps:  5,
			Messages:  []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "go"}}}},
			OnCompact: func(CompactInfo) { compactions++ },
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return client.getCalls(), compactions
	}

	containsReseed := func(calls []api.CreateMessageRequest) bool {
		for _, c := range calls {
			for _, m := range c.Messages {
				for _, b := range m.Content {
					if strings.Contains(b.Text, reseedMarker) {
						return true
					}
				}
			}
		}
		return false
	}

	t.Run("todo_write present reseeds", func(t *testing.T) {
		calls, compactions := run(t, "todo_write")
		if compactions == 0 {
			t.Fatal("fixture did not compact — reseed unobservable")
		}
		if !containsReseed(calls) {
			t.Error("no request carried the todo-reseed turn after compaction")
		}
	})

	t.Run("no todo tool no reseed", func(t *testing.T) {
		calls, compactions := run(t, "bloat_tool")
		if compactions == 0 {
			t.Fatal("fixture did not compact — negative control unobservable")
		}
		if containsReseed(calls) {
			t.Error("reseed turn appeared without a todo_write tool")
		}
	})
}

// TestGenerateTextDirect_DefaultMaxSteps pins the implicit step cap:
// MaxSteps <= 0 falls back to 10 tool-loop iterations.
func TestGenerateTextDirect_DefaultMaxSteps(t *testing.T) {
	scripts := make([][]api.StreamEvent, 12)
	for i := range scripts {
		scripts[i] = toolUseEvents(fmt.Sprintf("tu_%d", i), "noop", `{}`, 10, 5)
	}
	client := newMockClient(scripts...)
	noop, execCalls := charNoopTool("noop")

	res, err := GenerateTextDirect(context.Background(), client, GenerationOptions{
		Model:    "claude-sonnet-4-6",
		Tools:    []GenerationTool{noop},
		MaxSteps: 0,
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "loop"}}}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := len(client.getCalls()); got != 10 {
		t.Errorf("model calls = %d, want 10 (defaultMaxSteps)", got)
	}
	if len(res.Steps) != 10 {
		t.Errorf("Steps = %d, want 10", len(res.Steps))
	}
	if *execCalls != 10 {
		t.Errorf("tool executions = %d, want 10", *execCalls)
	}
	if res.FinishReason != FinishToolCalls {
		t.Errorf("FinishReason = %q, want %q (loop exhausted mid-tool-use)", res.FinishReason, FinishToolCalls)
	}
}

// TestGenerateTextDirect_UsageAccumulationAcrossSteps pins accumulateUsage's
// aggregation: input/output/cache tokens sum across steps and TotalTokens is
// recomputed as input+output.
func TestGenerateTextDirect_UsageAccumulationAcrossSteps(t *testing.T) {
	step1 := []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: 100, CacheReadInputTokens: 40, CacheCreationInputTokens: 10},
		{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "tool_use", Index: 0, ID: "tu_1", Name: "noop"}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "input_json_delta", PartialJSON: `{}`}},
		{Type: api.EventContentBlockStop, Index: 0},
		{Type: api.EventMessageDelta, StopReason: "tool_use", Usage: api.UsageDelta{OutputTokens: 30}},
		{Type: api.EventMessageStop},
	}
	step2 := []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: 50, CacheReadInputTokens: 5, CacheCreationInputTokens: 2},
		{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "text", Index: 0}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "text_delta", Text: "done"}},
		{Type: api.EventContentBlockStop, Index: 0},
		{Type: api.EventMessageDelta, StopReason: "end_turn", Usage: api.UsageDelta{OutputTokens: 20}},
		{Type: api.EventMessageStop},
	}
	client := newMockClient(step1, step2)
	noop, _ := charNoopTool("noop")

	res, err := GenerateTextDirect(context.Background(), client, GenerationOptions{
		Model:    "claude-sonnet-4-6",
		Tools:    []GenerationTool{noop},
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "go"}}}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	u := res.TotalUsage
	if u.InputTokens != 150 {
		t.Errorf("InputTokens = %d, want 150", u.InputTokens)
	}
	if u.OutputTokens != 50 {
		t.Errorf("OutputTokens = %d, want 50", u.OutputTokens)
	}
	if u.TotalTokens != 200 {
		t.Errorf("TotalTokens = %d, want 200 (input+output)", u.TotalTokens)
	}
	if u.CacheReadTokens != 45 {
		t.Errorf("CacheReadTokens = %d, want 45", u.CacheReadTokens)
	}
	if u.CacheWriteTokens != 12 {
		t.Errorf("CacheWriteTokens = %d, want 12", u.CacheWriteTokens)
	}
}

// TestGenerateTextDirect_EmptyToolInputQuirks pins two coupled quirks for a
// tool_use block that streamed NO input JSON: (1) the replayed assistant
// message falls back to an empty-but-non-nil Input object (a nil Input
// confuses some providers); (2) executeToolsDirect treats the empty string
// as malformed JSON — the tool never executes and the model sees an isError
// tool_result — yet the loop continues to the next model turn.
func TestGenerateTextDirect_EmptyToolInputQuirks(t *testing.T) {
	events := []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: 10},
		{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "tool_use", Index: 0, ID: "tu_1", Name: "noop"}},
		// No input_json_delta at all → PartialJSON stays "".
		{Type: api.EventContentBlockStop, Index: 0},
		{Type: api.EventMessageDelta, StopReason: "tool_use", Usage: api.UsageDelta{OutputTokens: 5}},
		{Type: api.EventMessageStop},
	}
	client := newMockClient(events, textEvents("recovered", 10, 5))
	noop, execCalls := charNoopTool("noop")

	var cbErr error
	var cbToolUseID string
	res, err := GenerateTextDirect(context.Background(), client, GenerationOptions{
		Model:    "claude-sonnet-4-6",
		Tools:    []GenerationTool{noop},
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "go"}}}},
		OnToolCall: func(info ToolCallInfo) {
			cbErr = info.Error
			cbToolUseID = info.ToolUseID
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *execCalls != 0 {
		t.Errorf("tool executed %d times, want 0 (malformed empty input)", *execCalls)
	}
	if cbErr == nil || !strings.Contains(cbErr.Error(), "malformed tool input") {
		t.Errorf("OnToolCall error = %v, want malformed tool input", cbErr)
	}
	if cbToolUseID != "tu_1" {
		t.Errorf("OnToolCall ToolUseID = %q, want tu_1", cbToolUseID)
	}
	if res.Text != "recovered" {
		t.Errorf("Text = %q — loop must continue past the malformed call", res.Text)
	}

	calls := client.getCalls()
	if len(calls) != 2 {
		t.Fatalf("model calls = %d, want 2", len(calls))
	}
	// Replayed assistant turn: tool_use with empty-but-non-nil Input.
	assistant := calls[1].Messages[1]
	if assistant.Role != "assistant" || len(assistant.Content) != 1 {
		t.Fatalf("replayed assistant turn = %+v", assistant)
	}
	tu := assistant.Content[0]
	if tu.Type != "tool_use" || tu.ID != "tu_1" {
		t.Fatalf("replayed block = %+v", tu)
	}
	if tu.Input == nil {
		t.Error("replayed tool_use Input is nil, want empty non-nil object")
	}
	if len(tu.Input) != 0 {
		t.Errorf("replayed tool_use Input = %v, want empty", tu.Input)
	}
	// Tool result: isError with the malformed message.
	toolResult := calls[1].Messages[2].Content[0]
	if toolResult.Type != "tool_result" || !toolResult.IsError {
		t.Fatalf("tool_result = %+v, want isError", toolResult)
	}
	if len(toolResult.Content) != 1 || !strings.Contains(toolResult.Content[0].Text, "malformed tool input") {
		t.Errorf("tool_result content = %+v", toolResult.Content)
	}
}
