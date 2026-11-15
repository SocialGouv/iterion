package model

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The tool-step bound is the node execution's, not the attempt's (#3283,
// F3). Measured on a review-pr reviewer with `tool_max_steps: 30`: steps
// 1..26, a stream retry, 1..2, a retry, 1..30 — 58 tool turns, each retry
// handed the whole bound again while the session store replayed the dead
// attempt's history. Every test below drives a claw node through Execute on
// a scripted model and counts the tool turns that RAN (the probe tool's own
// executions), never the requests.

const budgetSchema = `{"type":"object","properties":{"verdict":{"type":"string"}},"required":["verdict"]}`

// budgetProbe is the node's one tool; ran counts its executions.
func budgetProbe() (delegate.ToolDef, *atomic.Int64) {
	ran := &atomic.Int64{}
	return delegate.ToolDef{
		Name:        "probe",
		Description: "read the workspace",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Execute: func(context.Context, json.RawMessage) (string, error) {
			ran.Add(1)
			return "observed", nil
		},
	}, ran
}

// offersTool reports whether a request offers the named tool: a tool-loop
// request offers the node's tools, the structured recovery pass offers only
// its schema tool.
func offersTool(req api.CreateMessageRequest, name string) bool {
	return slices.ContainsFunc(req.Tools, func(t api.Tool) bool { return t.Name == name })
}

// cutStream is a response the stream cuts before it finishes: the transient
// failure a retry re-issues.
func cutStream() []api.StreamEvent {
	return []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: 10},
		{Type: api.EventError, ErrorMessage: "unexpected EOF"},
	}
}

// budgetModel answers by request kind: a loop request calls probe, a
// recovery request answers through the schema tool. cut lists the 1-based
// requests the stream cuts, narrate the loop requests answered with prose
// and no tool call.
func budgetModel(cut, narrate map[int]bool) *funcClient {
	return &funcClient{fn: func(n int, req api.CreateMessageRequest) ([]api.StreamEvent, error) {
		switch {
		case cut[n]:
			return cutStream(), nil
		case offersTool(req, "probe") && narrate[n]:
			return textEvents("I will read the diff next.", 10, 5), nil
		case offersTool(req, "probe"):
			return toolUseEvents(fmt.Sprintf("tu_%d", n), "probe", `{}`, 10, 5), nil
		}
		return toolUseEvents("so_1", "structured_output", `{"verdict":"done"}`, 10, 5), nil
	}}
}

// requestKinds renders the requests a client received, in order: "L" for a
// tool-loop request, "R" for a recovery request.
func requestKinds(c *funcClient) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	kinds := make([]byte, len(c.calls))
	for i, req := range c.calls {
		kinds[i] = 'R'
		if offersTool(req, "probe") {
			kinds[i] = 'L'
		}
	}
	return string(kinds)
}

// budgetBackend is a claw backend on the scripted client; its own retry loop
// takes attempts attempts on a transient failure.
func budgetBackend(client api.APIClient, attempts int) *ClawBackend {
	reg := NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) { return client, nil })
	return NewClawBackend(reg, EventHooks{}, RetryPolicy{MaxAttempts: attempts, BackoffBase: time.Millisecond})
}

func budgetTask(probe delegate.ToolDef, maxSteps int) delegate.Task {
	return delegate.Task{
		NodeID: "judge", Model: "test/test-model", UserPrompt: "Judge the diff.",
		HasTools: true, ToolDefs: []delegate.ToolDef{probe}, ToolMaxSteps: maxSteps,
		OutputSchema: json.RawMessage(budgetSchema),
	}
}

// budgetCtx wires the run's session store, so a retry replays the dead
// attempt's history the way a run does.
func budgetCtx() context.Context {
	return withRuntimeContext(context.Background(), "run-budget", newNodeSessionStore())
}

func requireVerdict(t *testing.T, res delegate.Result, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("the node must recover an answer: %v", err)
	}
	if got, _ := res.Output["verdict"].(string); got != "done" {
		t.Fatalf("output = %v, want the recovery pass's verdict", res.Output)
	}
}

// Two stream cuts mid-loop: 2 turns then a cut, 2 turns then a cut, and
// the third attempt is handed the 2 turns left — not the bound again — after
// which the loop is spent and the recovery pass answers, once.
//
// Mutant: hand every attempt the node's whole bound (a counter per attempt)
// and the third attempt runs 6 more turns: 10 for a bound of 6.
func TestClawStepBudget_StreamRetriesShareTheBound(t *testing.T) {
	t.Parallel()
	probe, ran := budgetProbe()
	client := budgetModel(map[int]bool{3: true, 6: true}, nil)
	res, err := budgetBackend(client, 3).Execute(budgetCtx(), budgetTask(probe, 6))
	requireVerdict(t, res, err)
	if got := ran.Load(); got != 6 {
		t.Fatalf("%d tool turns ran for tool_max_steps 6 — want 2 + 2 + the 2 left (requests %s)", got, requestKinds(client))
	}
	if got := requestKinds(client); got != "LLLLLLLLR" {
		t.Errorf("requests = %s, want 3 loop requests and a cut twice, then the 2 turns left and ONE recovery pass", got)
	}
	if res.ToolTurns != 6 {
		t.Errorf("Result.ToolTurns = %d, want the 6 every attempt completed", res.ToolTurns)
	}
}

// A cut before the first turn completes spends nothing: the retry gets the
// whole bound. Only COMPLETED tool turns count.
//
// Mutant: charge the request the stream cut as a turn, and the retry runs
// one turn short of the bound.
func TestClawStepBudget_ACutBeforeTheFirstTurnSpendsNothing(t *testing.T) {
	t.Parallel()
	probe, ran := budgetProbe()
	client := budgetModel(map[int]bool{1: true}, nil)
	res, err := budgetBackend(client, 3).Execute(budgetCtx(), budgetTask(probe, 4))
	requireVerdict(t, res, err)
	if got := ran.Load(); got != 4 {
		t.Fatalf("%d tool turns ran, want the whole bound of 4 after a cut that completed nothing (requests %s)", got, requestKinds(client))
	}
	if got := requestKinds(client); got != "LLLLLR" {
		t.Errorf("requests = %s, want the cut, the 4 turns, then one recovery pass", got)
	}
}

// Every allowed turn completes, then the recovery pass is cut and retried:
// the retry finds the bound spent and goes straight to the recovery pass,
// answering from the replayed conversation. It never calls the loop with
// nothing left — MaxSteps 0, which the loop reads as its own default of 10.
//
// Mutants: call the loop with the 0 left (10 more turns), or hand the retry
// the bound again (3 more turns) — either runs loop requests after the cut.
func TestClawStepBudget_AFailureAfterTheLastTurnGoesStraightToRecovery(t *testing.T) {
	t.Parallel()
	probe, ran := budgetProbe()
	client := budgetModel(map[int]bool{4: true}, nil)
	res, err := budgetBackend(client, 3).Execute(budgetCtx(), budgetTask(probe, 3))
	requireVerdict(t, res, err)
	if got := ran.Load(); got != 3 {
		t.Fatalf("%d tool turns ran for tool_max_steps 3 (requests %s)", got, requestKinds(client))
	}
	if got := requestKinds(client); got != "LLLRR" {
		t.Fatalf("requests = %s, want the 3 turns, the cut recovery pass, and the retry's recovery pass alone", got)
	}
	// The retry's recovery answers from the conversation the loop left: the
	// session store replays all three tool results into it.
	last := client.calls[len(client.calls)-1]
	results := 0
	for _, m := range last.Messages {
		for _, b := range m.Content {
			if b.Type == "tool_result" {
				results++
			}
		}
	}
	if results != 3 {
		t.Errorf("the retry's recovery pass carries %d tool results, want the 3 the loop completed", results)
	}
}

// The tool-use nudge spends from the same bound. Attempt 1 completes 3 of 4
// turns and is cut; attempt 2's first pass narrates without a tool call, so
// the nudge re-runs the loop — with the ONE turn left, after which the
// recovery pass answers.
//
// Mutant: hand the nudge the options' MaxSteps (the node's bound) instead of
// what is left, and it runs 4 turns: 7 for a bound of 4.
func TestClawStepBudget_TheNudgeSpendsFromTheSameBound(t *testing.T) {
	t.Parallel()
	probe, ran := budgetProbe()
	client := budgetModel(map[int]bool{4: true}, map[int]bool{5: true})
	res, err := budgetBackend(client, 3).Execute(budgetCtx(), budgetTask(probe, 4))
	requireVerdict(t, res, err)
	if got := ran.Load(); got != 4 {
		t.Fatalf("%d tool turns ran for tool_max_steps 4 — want 3, then the nudge's 1 (requests %s)", got, requestKinds(client))
	}
	if got := requestKinds(client); got != "LLLLLLR" {
		t.Errorf("requests = %s, want 3 turns, the cut, the narration, the nudge's one turn, one recovery pass", got)
	}
	if res.ToolTurns != 4 {
		t.Errorf("Result.ToolTurns = %d, want 4 — the nudge's turn is the node's too", res.ToolTurns)
	}
}

// An in-place retry of the EXECUTOR re-enters Execute on the conversation
// the dead attempt left (the session store survives it), carrying the
// anchor the first attempt started from, so the turns that replay holds
// stay spent: claw's own retry loop takes one attempt here, the executor's
// takes the cut, and its second attempt is handed the 3 turns left of 5.
//
// Mutant: drop the anchor's carry at the dispatch, and the second attempt
// fixes its anchor from its own replay — the dead turns included — and runs
// 5 turns: 7 for a bound of 5.
func TestClawStepBudget_AnExecutorRetryContinuesTheSpentTurns(t *testing.T) {
	t.Parallel()
	probe, ran := budgetProbe()
	client := budgetModel(map[int]bool{3: true}, nil)
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaw, budgetBackend(client, 1))
	e := newFallbackExecutor(reg, EventHooks{})
	build := e.newElementBuilder("judge", delegate.BackendClaw, nil,
		func(context.Context, string) (*delegate.Task, error) {
			task := budgetTask(probe, 5)
			return &task, nil
		})
	out, err := e.dispatchChain(budgetCtx(), "judge", []chainElement{{Label: "primary"}}, "test/test-model", build)
	requireVerdict(t, out.Result, err)
	if got := ran.Load(); got != 5 {
		t.Fatalf("%d tool turns ran for tool_max_steps 5 — want 2, then the 3 left (requests %s)", got, requestKinds(client))
	}
	if got := requestKinds(client); got != "LLLLLLR" {
		t.Errorf("requests = %s, want 2 turns and the cut, then the 3 turns left and one recovery pass", got)
	}
	if out.Result.ToolTurns != 5 {
		t.Errorf("Result.ToolTurns = %d, want the attempts' 2 + 3 folded", out.Result.ToolTurns)
	}
}

// storedRounds is a stored conversation holding n completed probe turns.
func storedRounds(n int) []api.Message {
	conversation := []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "Judge the diff."}}}}
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("tu_stored_%d", i)
		conversation = append(conversation,
			api.Message{Role: "assistant", Content: []api.ContentBlock{{Type: "tool_use", ID: id, Name: "probe", Input: map[string]any{}}}},
			api.Message{Role: "user", Content: []api.ContentBlock{api.ToolResult{ToolUseID: id, Content: "observed"}.ToContentBlock()}},
		)
	}
	return conversation
}

// toolResultsIn counts the tool results a request carries.
func toolResultsIn(req api.CreateMessageRequest) int {
	n := 0
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == "tool_result" {
				n++
			}
		}
	}
	return n
}

// A re-entered execution — an executor retry, a sandboxed runner's crossing,
// the engine's recovery re-run — is charged the turns its replay holds after
// the anchor it carries, never a count kept beside the conversation: a
// replay that holds the whole bound goes straight to the recovery pass with
// those turns in view, and one that lost the dead attempt's turns gets the
// whole bound back. An anchor a compaction folded away leaves only turns of
// this execution in view, all charged. Without an anchor the first
// conversation fixes it: a persisted session's earlier turns charge
// nothing.
//
// Mutants: charge every visible turn whatever the anchor (the anchor read
// as absent: a persisted session's turns are charged); charge none when the
// anchor is not found (the folded case runs the whole bound again).
func TestClawStepBudget_ARetryIsChargedWhatItsReplayHolds(t *testing.T) {
	t.Parallel()
	anchor := func(s string) *string { return &s }
	for _, tc := range []struct {
		name    string
		anchor  *string
		stored  int
		want    int
		kinds   string
		results int // tool results the answering request carries
	}{
		{name: "3 replayed of 5", anchor: anchor(""), stored: 3, want: 2, kinds: "LLR", results: 5},
		{name: "the whole bound replayed", anchor: anchor(""), stored: 5, want: 0, kinds: "R", results: 5},
		{name: "an anchor inside the replay", anchor: anchor("tu_stored_2"), stored: 5, want: 2, kinds: "LLR", results: 7},
		{name: "an anchor a compaction folded", anchor: anchor("tu_folded"), stored: 3, want: 2, kinds: "LLR", results: 5},
		{name: "the replay lost", anchor: anchor("tu_stored_4"), stored: 0, want: 5, kinds: "LLLLLR", results: 5},
		{name: "no anchor: the first conversation fixes it", anchor: nil, stored: 5, want: 5, kinds: "LLLLLR", results: 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			probe, ran := budgetProbe()
			client := budgetModel(nil, nil)
			task := budgetTask(probe, 5)
			task.ToolTurnsAnchor = tc.anchor
			store := newNodeSessionStore()
			store.save("run-budget", "judge", storedRounds(tc.stored))
			res, err := budgetBackend(client, 1).Execute(withRuntimeContext(context.Background(), "run-budget", store), task)
			requireVerdict(t, res, err)
			if got := ran.Load(); got != int64(tc.want) {
				t.Fatalf("%d tool turns ran with %d replayed, want %d (requests %s)", got, tc.stored, tc.want, requestKinds(client))
			}
			if got := requestKinds(client); got != tc.kinds {
				t.Errorf("requests = %s, want %s", got, tc.kinds)
			}
			last := client.calls[len(client.calls)-1]
			if got := toolResultsIn(last); got != tc.results {
				t.Errorf("the answering request carries %d tool results, want %d", got, tc.results)
			}
			if res.ToolTurns != tc.want {
				t.Errorf("Result.ToolTurns = %d, want this invocation's %d", res.ToolTurns, tc.want)
			}
			wantAnchor := lastCompletedToolCall(storedRounds(tc.stored))
			if tc.anchor != nil {
				wantAnchor = *tc.anchor
			}
			if res.ToolTurnsAnchor == nil || *res.ToolTurnsAnchor != wantAnchor {
				t.Errorf("Result.ToolTurnsAnchor = %v, want %q", res.ToolTurnsAnchor, wantAnchor)
			}
		})
	}
}

// A node without an output schema whose replay already holds the whole
// bound ends as its loop ends at the bound — with the text of the last turn
// — and makes no request: running the loop again would spend past the
// bound, and a failure would only have the engine retry it.
//
// Mutant: run the loop with the 0 left (MaxSteps 0, the loop's default of
// 10) and the node runs 10 more turns.
func TestClawStepBudget_ASchemalessNodeWithTheBoundReplayedEndsAtTheBound(t *testing.T) {
	t.Parallel()
	probe, ran := budgetProbe()
	client := budgetModel(nil, nil)
	task := budgetTask(probe, 5)
	task.OutputSchema = nil
	task.ToolTurnsAnchor = new(string)
	stored := storedRounds(5)
	stored[len(stored)-2].Content = append([]api.ContentBlock{{Type: "text", Text: "the last word"}}, stored[len(stored)-2].Content...)
	store := newNodeSessionStore()
	store.save("run-budget", "judge", stored)
	res, err := budgetBackend(client, 1).Execute(withRuntimeContext(context.Background(), "run-budget", store), task)
	if err != nil {
		t.Fatalf("the node must end at the bound, not fail: %v", err)
	}
	if got := res.Output["text"]; got != "the last word" {
		t.Errorf("output text = %v, want the last turn's", got)
	}
	if ran.Load() != 0 || client.count() != 0 {
		t.Fatalf("%d tool turns, %d requests — want none", ran.Load(), client.count())
	}
}

// A task that carries its own history — a resumed pause, a continued
// conversation — skips the store's replay, so a retry used to restart from
// that history while being charged the dead attempt's turns: the recovery
// pass answered with none of them in view. A retry now replays the dead
// attempt's capture when the store holds one that extends the carried
// history, and sees the turns it is charged for; with no store nothing is
// replayed, nothing is charged, and the retry answers from turns it ran
// itself.
//
// Each case completes the 4 turns of its bound, then the recovery pass is
// cut and the turn retried.
//
// Mutants: keep the carried history over its capture (the store case runs
// 4 more turns); charge the invocation's own count instead of the replay
// (the case without a store answers with none of its turns in view).
func TestClawStepBudget_ACarriedHistoryIsRetriedFromItsCapture(t *testing.T) {
	t.Parallel()
	resumed := func(task *delegate.Task) {
		prior, _ := json.Marshal([]api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "Judge the diff."}}},
			{Role: "assistant", Content: []api.ContentBlock{{Type: "tool_use", ID: "tu_ask", Name: "ask_user", Input: map[string]any{"question": "go?"}}}},
		})
		task.ResumeConversation, task.ResumePendingToolUseID, task.ResumeAnswer = prior, "tu_ask", "yes"
	}
	continued := func(task *delegate.Task) {
		prior, _ := json.Marshal([]api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "Judge the diff."}}},
			{Role: "assistant", Content: []api.ContentBlock{{Type: "text", Text: "verdict: maybe"}}},
		})
		task.ContinueConversation = prior
	}
	for _, tc := range []struct {
		name    string
		carry   func(*delegate.Task)
		store   bool
		ran     int64
		kinds   string
		results int // tool results the answering request carries
	}{
		{name: "resumed, with its capture", carry: resumed, store: true, ran: 4, kinds: "LLLLRR", results: 5},
		{name: "continued, with its capture", carry: continued, store: true, ran: 4, kinds: "LLLLRR", results: 4},
		{name: "resumed, no store", carry: resumed, ran: 8, kinds: "LLLLRLLLLR", results: 5},
		{name: "continued, no store", carry: continued, ran: 8, kinds: "LLLLRLLLLR", results: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			probe, ran := budgetProbe()
			client := budgetModel(map[int]bool{5: true}, nil)
			task := budgetTask(probe, 4)
			tc.carry(&task)
			ctx := context.Background()
			if tc.store {
				ctx = budgetCtx()
			}
			res, err := budgetBackend(client, 3).Execute(ctx, task)
			requireVerdict(t, res, err)
			if got := ran.Load(); got != tc.ran {
				t.Fatalf("%d tool turns ran, want %d (requests %s)", got, tc.ran, requestKinds(client))
			}
			if got := requestKinds(client); got != tc.kinds {
				t.Errorf("requests = %s, want %s", got, tc.kinds)
			}
			if got := toolResultsIn(client.calls[len(client.calls)-1]); got != tc.results {
				t.Errorf("the answering request carries %d tool results, want %d — the answer is grounded in turns it cannot see", got, tc.results)
			}
		})
	}
}

// The prefix test the carried-history replay rests on: a capture replaces
// the carried history only when it holds every message of it unchanged,
// and more.
func TestConversationExtends(t *testing.T) {
	t.Parallel()
	text := func(s string) api.Message {
		return api.Message{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: s}}}
	}
	carried := []api.Message{text("a"), text("b")}
	for _, tc := range []struct {
		name   string
		longer []api.Message
		want   bool
	}{
		{"one more", []api.Message{text("a"), text("b"), text("c")}, true},
		{"the same", []api.Message{text("a"), text("b")}, false},
		{"shorter", []api.Message{text("a")}, false},
		{"a changed message", []api.Message{text("a"), text("B"), text("c")}, false},
		{"none", nil, false},
	} {
		if got := conversationExtends(tc.longer, carried); got != tc.want {
			t.Errorf("%s: conversationExtends = %v, want %v", tc.name, got, tc.want)
		}
	}
	if conversationExtends([]api.Message{text("a")}, nil) {
		t.Error("an empty carried history is extended by a stored conversation — the store's turns would replace a history the task never had")
	}
}

// The launcher predicts the anchor of a sandboxed task that carries its own
// history from the task alone (carriedConversation), and the in-container
// loop must agree with it: a resumed pause whose answered call completes a
// turn, a continued conversation as it ended. The in-process Execute fixes
// its anchor from the conversation it composes — the two readings are
// pinned equal here.
//
// Mutant: compose a resumed pause without its answer, and the prediction
// anchors at the turn before.
func TestCarriedConversationMatchesTheComposedConversation(t *testing.T) {
	t.Parallel()
	prior := storedRounds(2)
	paused := append(append([]api.Message(nil), prior...),
		api.Message{Role: "assistant", Content: []api.ContentBlock{{Type: "tool_use", ID: "tu_ask", Name: "ask_user", Input: map[string]any{"question": "go?"}}}})
	resume, _ := json.Marshal(paused)
	cont, _ := json.Marshal(append(append([]api.Message(nil), prior...),
		api.Message{Role: "assistant", Content: []api.ContentBlock{{Type: "text", Text: "verdict: maybe"}}}))
	for _, tc := range []struct {
		name  string
		carry func(*delegate.Task)
		want  string
	}{
		{"resumed", func(task *delegate.Task) {
			task.ResumeConversation, task.ResumePendingToolUseID, task.ResumeAnswer = resume, "tu_ask", "yes"
		}, "tu_ask"},
		{"continued", func(task *delegate.Task) { task.ContinueConversation = cont }, "tu_stored_2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			probe, _ := budgetProbe()
			task := budgetTask(probe, 4)
			tc.carry(&task)
			if got := lastCompletedToolCall(carriedConversation(task)); got != tc.want {
				t.Fatalf("the launcher's anchor = %q, want %q", got, tc.want)
			}
			res, err := budgetBackend(budgetModel(nil, nil), 1).Execute(context.Background(), task)
			requireVerdict(t, res, err)
			if res.ToolTurnsAnchor == nil || *res.ToolTurnsAnchor != tc.want {
				t.Fatalf("the in-process loop fixed anchor %v, the launcher predicts %q", res.ToolTurnsAnchor, tc.want)
			}
		})
	}
}

// anchorRecordingBackend records the tool-step anchor each call is handed:
// call 1 answers without the schema's field, call 2 (the re-ask's first
// attempt) fixes anchor "tu_3" and dies on a transient failure, call 3
// answers.
type anchorRecordingBackend struct {
	mu      sync.Mutex
	anchors []*string
}

func (b *anchorRecordingBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.anchors = append(b.anchors, task.ToolTurnsAnchor)
	switch len(b.anchors) {
	case 1:
		return delegate.Result{Output: map[string]any{"other": "x"}, BackendName: "test_backend"}, nil
	case 2:
		anchor := "tu_3"
		return delegate.Result{ToolTurnsAnchor: &anchor, ToolTurns: 2, BackendName: "test_backend"}, &delegate.ErrTransient{Reason: "stream closed"}
	}
	return delegate.Result{Output: map[string]any{"answer": "blue"}, BackendName: "test_backend"}, nil
}

// A schema re-ask is a conversation of its own — a restart runs the node's
// tool loop again — and its in-place retries are charged against the anchor
// its first attempt fixed, like the dispatch's own: the retry is handed it.
//
// Mutant: run the re-ask's retries without the anchor's carry, and the
// retry is handed none — it would fix one from its own replay.
func TestToolTurnsAnchorRidesTheReaskRetries(t *testing.T) {
	t.Parallel()
	backend := &anchorRecordingBackend{}
	reg := delegate.NewRegistry()
	reg.Register("test_backend", backend)
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{
		"out_schema": {Name: "out_schema", Fields: []*ir.SchemaField{{Name: "answer", Type: ir.FieldTypeString}}},
	}}
	exec := NewClawExecutor(NewRegistry(), wf, WithBackendRegistry(reg),
		WithRetryPolicy(RetryPolicy{MaxAttempts: 3, BackoffBase: time.Millisecond}))
	node := &ir.AgentNode{
		BaseNode:     ir.BaseNode{ID: "answerer"},
		LLMFields:    ir.LLMFields{Backend: "test_backend"},
		SchemaFields: ir.SchemaFields{OutputSchema: "out_schema"},
	}
	output, err := exec.executeBackend(context.Background(), node, map[string]any{})
	if err != nil || output["answer"] != "blue" {
		t.Fatalf("the re-ask's retry must answer: output=%v err=%v", output, err)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.anchors) != 3 {
		t.Fatalf("%d calls, want the answer, the re-ask and its retry", len(backend.anchors))
	}
	if backend.anchors[1] != nil {
		t.Errorf("the re-ask's first attempt was handed anchor %q, want none (it fixes its own)", *backend.anchors[1])
	}
	if got := backend.anchors[2]; got == nil || *got != "tu_3" {
		t.Fatalf("the re-ask's retry was handed anchor %v, want the tu_3 its first attempt fixed", got)
	}
}

// A node execution ends when the node completes: the anchor kept beside its
// session goes with it, so the next execution of a persisted session —
// whose conversation the store still holds — fixes an anchor of its own and
// is charged none of the earlier execution's turns.
//
// Mutant: keep the anchor past the node's completion, and the second
// execution is handed the first one's anchor.
func TestAnEndedNodeExecutionLeavesNoAnchor(t *testing.T) {
	t.Parallel()
	var handed []*string
	e, n := newPersistFallbackExecutor(t, func(_ context.Context, task delegate.Task) (delegate.Result, error) {
		handed = append(handed, task.ToolTurnsAnchor)
		res := persistedResult(task)
		anchor := fmt.Sprintf("tu_execution_%d", len(handed))
		res.ToolTurnsAnchor = &anchor
		return res, nil
	})
	ctx := WithRunID(context.Background(), "run")
	for range 2 {
		if _, err := e.Execute(ctx, n, map[string]any{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(handed) != 2 || handed[0] != nil {
		t.Fatalf("calls %d, first anchor %v — want two executions, the first unanchored", len(handed), handed)
	}
	if handed[1] != nil {
		t.Errorf("the second execution was handed the first one's anchor %q", *handed[1])
	}
	if kept := e.sessions.anchorOf("run", "conversation"); kept != nil {
		t.Errorf("anchor %q kept beside the session after the node completed", *kept)
	}
}
