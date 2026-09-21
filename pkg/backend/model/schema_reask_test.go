package model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The schema re-ask (executor_reask.go) is the one more turn a node gets when
// its answer fails its output schema on a fixable shape. These tests hold what
// each mode SENDS the model and what the run observes of it — a re-ask that
// quietly repeated the whole turn, with the pause it resumed from replayed
// inside it and no feedback at all, is what left copilot's wave-4 run
// failed_resumable after 83k tokens (#1385).

const reaskPrompt = "Judge the diff and say why."

// verdictWorkflow is a two-field schema and a user prompt whose text the
// assertions can count in a request.
func verdictWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Prompts: map[string]*ir.Prompt{"ask": {Body: reaskPrompt}},
		Schemas: map[string]*ir.Schema{"verdict_schema": {
			Name: "verdict_schema",
			Fields: []*ir.SchemaField{
				{Name: "verdict", Type: ir.FieldTypeBool},
				{Name: "reason", Type: ir.FieldTypeString},
			},
		}},
	}
}

func verdictJudge(backend string) *ir.JudgeNode {
	return &ir.JudgeNode{
		BaseNode:     ir.BaseNode{ID: "judge"},
		LLMFields:    ir.LLMFields{Backend: backend, Model: "test/test-model", UserPrompt: "ask"},
		SchemaFields: ir.SchemaFields{OutputSchema: "verdict_schema"},
	}
}

// reaskObserver records the delegate lifecycle hooks around a re-ask.
type reaskObserver struct {
	retries, started, finished, failed []DelegateInfo
}

func (o *reaskObserver) hooks() EventHooks {
	return EventHooks{
		OnDelegateRetry:    func(_ string, i DelegateInfo) { o.retries = append(o.retries, i) },
		OnDelegateStarted:  func(_ string, i DelegateInfo) { o.started = append(o.started, i) },
		OnDelegateFinished: func(_ string, i DelegateInfo) { o.finished = append(o.finished, i) },
		OnDelegateError:    func(_ string, i DelegateInfo) { o.failed = append(o.failed, i) },
	}
}

func withAttempt(infos []DelegateInfo, attempt int) []DelegateInfo {
	var out []DelegateInfo
	for _, i := range infos {
		if i.Attempt == attempt {
			out = append(out, i)
		}
	}
	return out
}

// textOccurrences counts needle across every text block of a conversation.
func textOccurrences(msgs []api.Message, needle string) int {
	n := 0
	for _, m := range msgs {
		for _, b := range m.Content {
			n += strings.Count(b.Text, needle)
		}
	}
	return n
}

func toolResultBlocks(msgs []api.Message) int {
	n := 0
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == "tool_result" {
				n++
			}
		}
	}
	return n
}

func lastUserText(t *testing.T, msgs []api.Message) string {
	t.Helper()
	if len(msgs) == 0 {
		t.Fatal("empty conversation")
	}
	last := msgs[len(msgs)-1]
	if last.Role != "user" || len(last.Content) == 0 {
		t.Fatalf("the conversation does not end on a user turn: %+v", last)
	}
	return last.Content[len(last.Content)-1].Text
}

// On claw the re-ask CONTINUES the conversation the node just completed:
// the same messages, the answer the model gave, then the validation error as
// the next user turn — a single schema-forced call with no tools. Mutating
// the mode to a restart puts the prompt back in the last turn (twice in the
// conversation), which is what the two occurrence assertions refuse.
func TestSchemaReask_ClawContinuesTheCompletedConversation(t *testing.T) {
	mock := newMockClient(
		toolUseEvents("tu_1", "structured_output", `{"verdict":true}`, 50, 20),
		toolUseEvents("tu_2", "structured_output", `{"verdict":true,"reason":"because"}`, 30, 10),
	)
	reg := NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) { return mock, nil })
	obs := &reaskObserver{}
	exec := newTestClawExecutor(reg, verdictWorkflow(), WithEventHooks(obs.hooks()))

	// Execute, not executeBackend: it is what wires the run's session store
	// into ctx — the store the continuation reads.
	output, err := exec.Execute(WithRunID(context.Background(), "run-reask"), verdictJudge(""), map[string]any{})
	if err != nil {
		t.Fatalf("the re-ask was supposed to deliver the node: %v", err)
	}
	if output["reason"] != "because" {
		t.Fatalf("output = %v, want the re-asked answer", output)
	}
	calls := mock.getCalls()
	if len(calls) != 2 {
		t.Fatalf("expected the answer and one re-ask, got %d provider calls", len(calls))
	}
	first, second := calls[0].Messages, calls[1].Messages
	if len(second) != len(first)+2 {
		t.Fatalf("the re-ask does not continue the conversation: %d messages after a first request of %d (want the answer + the feedback turn)", len(second), len(first))
	}
	last := lastUserText(t, second)
	if !strings.Contains(last, schemaRetryFeedbackMarker) || !strings.Contains(last, "reason") {
		t.Errorf("the re-ask's user turn does not carry the validation error: %q", last)
	}
	if strings.Contains(last, reaskPrompt) {
		t.Errorf("the re-ask repeated the prompt instead of asking for the fix: %q", last)
	}
	if got := textOccurrences(second, reaskPrompt); got != 1 {
		t.Errorf("the prompt appears %d times in the re-ask's conversation, want exactly once", got)
	}
	if len(calls[1].Tools) != 1 || calls[1].Tools[0].Name != "structured_output" {
		t.Errorf("the re-ask offers tools beyond the schema's: %+v", calls[1].Tools)
	}

	if len(obs.retries) != 1 || obs.retries[0].Reask != ReaskContinueConversation || obs.retries[0].Attempt != 1 {
		t.Errorf("delegate_retry did not announce the continuation: %+v", obs.retries)
	}
	if s := withAttempt(obs.started, 2); len(s) != 1 || s[0].Reask != ReaskContinueConversation {
		t.Errorf("the re-ask has no delegate_started of its own: %+v", obs.started)
	}
	f := withAttempt(obs.finished, 2)
	if len(f) != 1 || f[0].Reask != ReaskContinueConversation || f[0].Tokens != 40 {
		t.Errorf("the re-ask has no delegate_finished of its own carrying its own tokens (40): %+v", f)
	}
	if len(withAttempt(obs.finished, 0)) != 1 {
		t.Errorf("the delegation's own delegate_finished must fire exactly once, got %+v", obs.finished)
	}
	if got, _ := output["_tokens"].(int); got != 110 {
		t.Errorf("_tokens = %v, want 110 — both turns on the node's bill", output["_tokens"])
	}
}

// pauseConversation is a claw conversation captured at an ask_user pause:
// the prompt, then the assistant's pending tool_use.
func pauseConversation(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal([]api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: reaskPrompt}}},
		{Role: "assistant", Content: []api.ContentBlock{{Type: "tool_use", ID: "tu_ask", Name: "ask_user", Input: map[string]any{"question": "may I?"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The shape of #1385: a node re-invoked after a pause (its task replays the
// paused conversation), whose post-pause turn ends on JSON missing a field.
// The re-ask must continue from where that turn ENDED — the resumed
// conversation once, the answer, the feedback — never replay the pause a
// second time behind the stored history (measured: 91 + 11 messages) and
// never drop the feedback (the resume form discards the prompt).
func TestSchemaReask_ResumedClawNodeGetsTheFeedbackWithoutReplayingThePause(t *testing.T) {
	mock := newMockClient(
		// The post-pause turn: the model answers on the spot, JSON lacking `reason`.
		textEvents(`{"verdict":true}`, 50, 20),
		toolUseEvents("tu_2", "structured_output", `{"verdict":true,"reason":"because"}`, 30, 10),
	)
	reg := NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) { return mock, nil })
	tr := tool.NewRegistry()
	// read_file is the node's own tool; todo_write is the one the executor
	// adds to every tool-bearing claw node.
	for _, name := range []string{"read_file", "todo_write"} {
		if err := tr.RegisterBuiltin(name, "test tool "+name, nil, jsonExec(`{"ok":true}`)); err != nil {
			t.Fatal(err)
		}
	}
	obs := &reaskObserver{}
	exec := newTestClawExecutor(reg, verdictWorkflow(), WithToolRegistry(tr), WithEventHooks(obs.hooks()))
	node := verdictJudge("")
	node.Tools = []string{"read_file"}
	input := map[string]any{
		delegate.ResumeConversationKey:     pauseConversation(t),
		delegate.ResumePendingToolUseIDKey: "tu_ask",
		delegate.ResumeAnswerKey:           "yes",
	}

	output, err := exec.Execute(WithRunID(context.Background(), "run-resumed"), node, input)
	if err != nil {
		t.Fatalf("the re-ask was supposed to deliver the node: %v", err)
	}
	if output["reason"] != "because" {
		t.Fatalf("output = %v, want the re-asked answer", output)
	}
	calls := mock.getCalls()
	if len(calls) != 2 {
		t.Fatalf("expected the post-pause turn and one re-ask, got %d provider calls", len(calls))
	}
	// The post-pause turn: the paused conversation plus the operator's
	// answer as its tool_result — 3 messages, nothing prepended.
	if len(calls[0].Messages) != 3 {
		t.Fatalf("the post-pause turn sent %d messages, want 3 (the pause + the answer)", len(calls[0].Messages))
	}
	second := calls[1].Messages
	// The re-ask: those 3, the assistant's answer, the feedback.
	if len(second) != 5 {
		t.Fatalf("the re-ask sent %d messages, want 5 (the resumed conversation once, the answer, the feedback)", len(second))
	}
	if got := toolResultBlocks(second); got != 1 {
		t.Errorf("the pause's answer appears %d times in the re-ask, want once", got)
	}
	if got := textOccurrences(second, reaskPrompt); got != 1 {
		t.Errorf("the prompt appears %d times in the re-ask, want exactly once", got)
	}
	// The tool loop returns its final text beside the messages; the re-ask
	// puts the answer back as the turn before the feedback, so the model
	// sees what it answered.
	if answer := second[3]; answer.Role != "assistant" || !strings.Contains(answer.Content[0].Text, `"verdict":true`) {
		t.Errorf("the model's own answer is not the turn before the feedback: %+v", answer)
	}
	if last := lastUserText(t, second); !strings.Contains(last, schemaRetryFeedbackMarker) {
		t.Errorf("the re-ask does not carry the validation error: %q", last)
	}
	if len(obs.retries) != 1 || obs.retries[0].Reask != ReaskContinueConversation {
		t.Errorf("delegate_retry did not announce a continuation: %+v", obs.retries)
	}
}

// The same composition — the store's capture prepended to a task that
// already replays its history — corrupted every transient retry of a
// resumed claw node (measured 11 → 22 → 33 messages on the primary route of
// the same run). A retry re-sends the resumed conversation, once.
func TestClawResumedNodeTransientRetryDoesNotReplayTheHistoryTwice(t *testing.T) {
	mock := newMockClient(
		// The first attempt dies mid-stream after the request went out, so
		// the partial result carries the request's messages — what the
		// store captures.
		[]api.StreamEvent{
			{Type: api.EventMessageStart, InputTokens: 10},
			{Type: api.EventError, ErrorMessage: "upstream overloaded"},
		},
		textEvents(`{"verdict":true,"reason":"because"}`, 30, 10),
	)
	reg := NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) { return mock, nil })
	backend := NewClawBackend(reg, EventHooks{}, RetryPolicy{MaxAttempts: 3, BackoffBase: time.Millisecond})
	schemaJSON, err := SchemaToJSON(verdictWorkflow().Schemas["verdict_schema"])
	if err != nil {
		t.Fatal(err)
	}
	task := delegate.Task{
		NodeID:                 "judge",
		Model:                  "test/test-model",
		UserPrompt:             reaskPrompt,
		OutputSchema:           schemaJSON,
		HasTools:               true,
		ToolDefs:               []delegate.ToolDef{{Name: "read_file", Description: "test tool", InputSchema: json.RawMessage(`{"type":"object"}`), Execute: jsonExec(`{}`)}},
		ResumeConversation:     pauseConversation(t),
		ResumePendingToolUseID: "tu_ask",
		ResumeAnswer:           "yes",
	}
	ctx := withRuntimeContext(context.Background(), "run-retry", newNodeSessionStore())
	if _, err := backend.Execute(ctx, task); err != nil {
		t.Fatalf("the retry was supposed to carry the call through: %v", err)
	}
	calls := mock.getCalls()
	if len(calls) != 2 {
		t.Fatalf("expected a failed attempt and its retry, got %d provider calls", len(calls))
	}
	if got, want := len(calls[1].Messages), len(calls[0].Messages); got != want {
		t.Fatalf("the retry sent %d messages, want %d — the resumed conversation once, not the failed attempt's capture prepended to it", got, want)
	}
}

// A backend that resumes by session id (claude_code, codex, pi) gets the
// validation error as the next prompt of the SESSION the answer ran in —
// resumed, never forked, never optional — with nothing of the original
// prompt repeated.
func TestSchemaReask_ResumesTheSessionOnBackendsThatResumeById(t *testing.T) {
	for _, backendName := range []string{delegate.BackendClaudeCode, delegate.BackendCodex} {
		t.Run(backendName, func(t *testing.T) {
			backend := &capturingBackend{results: []delegate.Result{
				{Output: map[string]any{"verdict": true}, SessionID: "sess-1", SessionFingerprint: "anthropic-oauth", BackendName: backendName},
				{Output: map[string]any{"verdict": true, "reason": "because"}, SessionID: "sess-1", BackendName: backendName},
			}}
			reg := delegate.NewRegistry()
			reg.Register(backendName, backend)
			obs := &reaskObserver{}
			exec := NewClawExecutor(NewRegistry(), verdictWorkflow(),
				WithBackendRegistry(reg),
				WithEventHooks(obs.hooks()),
				WithRetryPolicy(RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}),
			)
			output, err := exec.executeBackend(context.Background(), verdictJudge(backendName), map[string]any{})
			if err != nil {
				t.Fatalf("the re-ask was supposed to deliver the node: %v", err)
			}
			if output["reason"] != "because" {
				t.Fatalf("output = %v, want the re-asked answer", output)
			}
			if len(backend.tasks) != 2 {
				t.Fatalf("expected the answer and one re-ask, got %d delegations", len(backend.tasks))
			}
			reask := backend.tasks[1]
			if reask.SessionID != "sess-1" {
				t.Errorf("the re-ask does not resume the answer's session: %q", reask.SessionID)
			}
			if reask.ForkSession || reask.SessionOptional {
				t.Errorf("the re-ask must continue the session unconditionally: fork=%v optional=%v", reask.ForkSession, reask.SessionOptional)
			}
			if reask.SessionFingerprint != "anthropic-oauth" {
				t.Errorf("the re-ask does not carry the session's fingerprint: %q", reask.SessionFingerprint)
			}
			if !strings.Contains(reask.UserPrompt, schemaRetryFeedbackMarker) || strings.Contains(reask.UserPrompt, reaskPrompt) {
				t.Errorf("the re-ask's prompt must be the feedback alone: %q", reask.UserPrompt)
			}
			if len(obs.retries) != 1 || obs.retries[0].Reask != ReaskResumeSession {
				t.Errorf("delegate_retry did not announce a session resume: %+v", obs.retries)
			}
			if f := withAttempt(obs.finished, 2); len(f) != 1 || f[0].Reask != ReaskResumeSession {
				t.Errorf("the re-ask has no delegate_finished of its own: %+v", obs.finished)
			}
		})
	}
}

// Where nothing can be continued — a backend that never resumes a session,
// or one that reported no session id — the re-ask restarts the turn with the
// error appended to the prompt, and says so.
func TestSchemaReask_RestartsWhereNothingCanBeContinued(t *testing.T) {
	cases := []struct {
		name, backend, sessionID string
	}{
		{"kimi never resumes", delegate.BackendKimi, "sess-k"},
		{"codex without a session id", delegate.BackendCodex, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := &capturingBackend{results: []delegate.Result{
				{Output: map[string]any{"verdict": true}, SessionID: tc.sessionID, BackendName: tc.backend},
				{Output: map[string]any{"verdict": true, "reason": "because"}, BackendName: tc.backend},
			}}
			reg := delegate.NewRegistry()
			reg.Register(tc.backend, backend)
			obs := &reaskObserver{}
			exec := NewClawExecutor(NewRegistry(), verdictWorkflow(),
				WithBackendRegistry(reg),
				WithEventHooks(obs.hooks()),
				WithRetryPolicy(RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}),
			)
			output, err := exec.executeBackend(context.Background(), verdictJudge(tc.backend), map[string]any{})
			if err != nil {
				t.Fatalf("the re-ask was supposed to deliver the node: %v", err)
			}
			if output["reason"] != "because" {
				t.Fatalf("output = %v, want the re-asked answer", output)
			}
			reask := backend.tasks[1]
			if reask.SessionID != "" {
				t.Errorf("a restart must not name a session it cannot continue: %q", reask.SessionID)
			}
			if !strings.Contains(reask.UserPrompt, reaskPrompt) || !strings.Contains(reask.UserPrompt, schemaRetryFeedbackMarker) {
				t.Errorf("a restart sends the prompt with the feedback appended: %q", reask.UserPrompt)
			}
			if len(obs.retries) != 1 || obs.retries[0].Reask != ReaskRestart {
				t.Errorf("delegate_retry did not announce a restart: %+v", obs.retries)
			}
		})
	}
}

// A claw node with no captured conversation (no run wired the session
// store) restarts too — the floor, never a refusal.
func TestSchemaReask_ClawWithoutACapturedConversationRestarts(t *testing.T) {
	mock := newMockClient(
		toolUseEvents("tu_1", "structured_output", `{"verdict":true}`, 50, 20),
		toolUseEvents("tu_2", "structured_output", `{"verdict":true,"reason":"because"}`, 30, 10),
	)
	reg := NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) { return mock, nil })
	obs := &reaskObserver{}
	exec := newTestClawExecutor(reg, verdictWorkflow(), WithEventHooks(obs.hooks()))
	// No run id on ctx: no session store, nothing captured.
	output, err := exec.executeBackend(context.Background(), verdictJudge(""), map[string]any{})
	if err != nil {
		t.Fatalf("the re-ask was supposed to deliver the node: %v", err)
	}
	if output["reason"] != "because" {
		t.Fatalf("output = %v, want the re-asked answer", output)
	}
	if len(obs.retries) != 1 || obs.retries[0].Reask != ReaskRestart {
		t.Errorf("delegate_retry did not announce a restart: %+v", obs.retries)
	}
	last := lastUserText(t, mock.getCalls()[1].Messages)
	if !strings.Contains(last, reaskPrompt) || !strings.Contains(last, schemaRetryFeedbackMarker) {
		t.Errorf("a restart sends the prompt with the feedback appended: %q", last)
	}
}

// A re-ask that fails does not vanish behind the validation error it was
// answering: its own error travels beside it, typed — a usage window hit
// during the re-ask is still what the run-level retry keys on — and its
// delegate_error names the attempt.
func TestSchemaReask_SurfacesTheReaskFailureBesideTheValidationError(t *testing.T) {
	window := &delegate.ErrRateLimited{Provider: delegate.BackendClaudeCode, Kind: delegate.RateLimitKindUsageWindow, Detail: "weekly cap"}
	backend := &stubBackend{
		results: []delegate.Result{{Output: map[string]any{"verdict": true}, SessionID: "sess-1", BackendName: delegate.BackendClaudeCode}},
		errors:  []error{nil, window},
	}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, backend)
	obs := &reaskObserver{}
	exec := NewClawExecutor(NewRegistry(), verdictWorkflow(),
		WithBackendRegistry(reg),
		WithEventHooks(obs.hooks()),
		WithRetryPolicy(RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}),
	)
	_, err := exec.executeBackend(context.Background(), verdictJudge(delegate.BackendClaudeCode), map[string]any{})
	if err == nil {
		t.Fatal("precondition: a re-ask that fails must fail the node")
	}
	if !delegate.IsUsageWindow(err) {
		t.Errorf("the re-ask's typed failure is lost from the node's error: %v", err)
	}
	var rl *delegate.ErrRateLimited
	if !errors.As(err, &rl) || rl != window {
		t.Errorf("errors.As does not reach the re-ask's error: %v", err)
	}
	for _, want := range []string{"structured output invalid", `missing required field "reason"`, "re-ask failed", "weekly cap"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not carry %q", err, want)
		}
	}
	if f := withAttempt(obs.failed, 2); len(f) != 1 || f[0].Reask != ReaskResumeSession || f[0].Error == nil {
		t.Errorf("the re-ask has no delegate_error of its own: %+v", obs.failed)
	}
}

// A second identical answer is the model's verdict: the node fails on it,
// naming the field still missing, after exactly one re-ask.
func TestSchemaReask_AsksOnceThenFails(t *testing.T) {
	backend := &capturingBackend{results: []delegate.Result{
		{Output: map[string]any{"verdict": true}, SessionID: "sess-1", BackendName: delegate.BackendClaudeCode},
		{Output: map[string]any{"verdict": false}, SessionID: "sess-1", BackendName: delegate.BackendClaudeCode},
	}}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, backend)
	exec := NewClawExecutor(NewRegistry(), verdictWorkflow(),
		WithBackendRegistry(reg),
		WithRetryPolicy(RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}),
	)
	_, err := exec.executeBackend(context.Background(), verdictJudge(delegate.BackendClaudeCode), map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "structured output invalid after retry") || !strings.Contains(err.Error(), `"reason"`) {
		t.Fatalf("want the typed failure naming the missing field after one re-ask, got %v", err)
	}
	if len(backend.tasks) != 2 {
		t.Fatalf("the re-ask budget is one: got %d delegations", len(backend.tasks))
	}
}

// The LLM router is the other place a structured answer is judged; a route
// decided on an incomplete verdict is a run lost to the same missing field,
// so it gets the same single re-ask.
func TestLLMRouter_ReasksOnceOnAnIncompleteVerdict(t *testing.T) {
	backend := &stubBackend{results: []delegate.Result{
		{Output: map[string]any{"selected_route": "agent_a"}, Tokens: 100},
		{Output: map[string]any{"selected_route": "agent_a", "reasoning": "it fits"}, Tokens: 40},
	}}
	obs := &reaskObserver{}
	exec := newDelegateTestExecutor(backend, obs.hooks())
	node := &ir.RouterNode{
		BaseNode:   ir.BaseNode{ID: "pick"},
		LLMFields:  ir.LLMFields{Backend: "test_backend", SystemPrompt: "sys"},
		RouterMode: ir.RouterLLM,
	}
	output, err := exec.executeLLMRouterUnified(context.Background(), node, map[string]any{"_route_candidates": []string{"agent_a", "agent_b"}})
	if err != nil {
		t.Fatalf("the re-ask was supposed to deliver the route: %v", err)
	}
	if output["selected_route"] != "agent_a" || output["reasoning"] != "it fits" {
		t.Fatalf("output = %v, want the re-asked verdict", output)
	}
	if backend.calls != 2 {
		t.Fatalf("expected the verdict and one re-ask, got %d delegations", backend.calls)
	}
	if len(obs.retries) != 1 || obs.retries[0].Reask == "" {
		t.Errorf("delegate_retry did not announce the router's re-ask: %+v", obs.retries)
	}
	if got, _ := output["_tokens"].(int); got != 140 {
		t.Errorf("_tokens = %v, want 140 — both turns on the router's bill", output["_tokens"])
	}
}

// The re-ask's own delegate_finished carries what the re-ask ADDED: on a
// backend whose cost is a session total, the difference; elsewhere its own.
func TestReaskMarginalCost(t *testing.T) {
	first := delegate.Result{Output: map[string]any{"_cost_usd": 1.50}, CostIsSessionTotal: true}
	reask := delegate.Result{Output: map[string]any{"_cost_usd": 1.90}, CostIsSessionTotal: true}
	if got := reaskMarginalCost(first, reask, true); got < 0.399 || got > 0.401 {
		t.Errorf("session-total re-ask: marginal = %v, want 0.40", got)
	}
	perCall := delegate.Result{Output: map[string]any{"_cost_usd": 0.30}}
	if got := reaskMarginalCost(first, perCall, true); got != 0.30 {
		t.Errorf("per-call re-ask: marginal = %v, want its own 0.30", got)
	}
	if got := reaskMarginalCost(first, reask, false); got != 1.90 {
		t.Errorf("a re-ask in another session is its own figure: %v, want 1.90", got)
	}
}

// A transient error INSIDE the re-ask's own transport retry is still part of
// the re-ask: its delegate_retry carries the reask marker, so a timeline
// reader keys it to the re-ask's work instead of mistaking it for a
// first-attempt retry. The announce and the in-place retry are told apart by
// Delay — only the loop sets one. Mutating the marker away reddens the
// mid-re-ask assertion while the announce stays marked.
func TestSchemaReask_TransportRetryInsideTheReaskCarriesTheMarker(t *testing.T) {
	transient := errors.New("fetch failed")
	backend := &stubBackend{
		results: []delegate.Result{
			{Output: map[string]any{"verdict": true}, SessionID: "sess-1", BackendName: delegate.BackendClaudeCode},
			{Output: map[string]any{"verdict": true, "reason": "because"}, SessionID: "sess-1", BackendName: delegate.BackendClaudeCode},
			{Output: map[string]any{"verdict": true, "reason": "because"}, SessionID: "sess-1", BackendName: delegate.BackendClaudeCode},
		},
		errors: []error{nil, transient},
	}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, backend)
	obs := &reaskObserver{}
	exec := NewClawExecutor(NewRegistry(), verdictWorkflow(),
		WithBackendRegistry(reg),
		WithEventHooks(obs.hooks()),
		WithRetryPolicy(RetryPolicy{MaxAttempts: 2, BackoffBase: time.Millisecond}),
	)
	output, err := exec.executeBackend(context.Background(), verdictJudge(delegate.BackendClaudeCode), map[string]any{})
	if err != nil {
		t.Fatalf("the re-ask's transport retry was supposed to carry it: %v", err)
	}
	if output["reason"] != "because" {
		t.Fatalf("output = %v, want the re-asked answer", output)
	}
	announced, mid := 0, 0
	for _, r := range obs.retries {
		switch {
		case r.Reask == ReaskResumeSession && r.Delay == 0:
			announced++
		case r.Delay > 0 && errors.Is(r.Error, transient):
			mid++
			if r.Reask != ReaskResumeSession {
				t.Errorf("a mid-re-ask transport retry carries no reask marker: %+v", r)
			}
		}
	}
	if announced != 1 {
		t.Errorf("the re-ask announced itself %d times, want once: %+v", announced, obs.retries)
	}
	if mid == 0 {
		t.Fatalf("fixture produced no mid-re-ask transport retry: %+v", obs.retries)
	}
}
