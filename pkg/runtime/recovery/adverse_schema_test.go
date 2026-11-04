package recovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

const rvaPrompt = "Judge the diff and say why."
const rvaFeedbackMarker = "[OUTPUT SCHEMA VALIDATION FAILED]"

type rvaScriptClient struct {
	mu      sync.Mutex
	scripts [][]api.StreamEvent
	calls   []api.CreateMessageRequest
}

func (m *rvaScriptClient) StreamResponse(_ context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, req)
	if len(m.scripts) == 0 {
		return nil, fmt.Errorf("rva mock: no more scripts")
	}
	s := m.scripts[0]
	m.scripts = m.scripts[1:]
	ch := make(chan api.StreamEvent, len(s))
	for _, ev := range s {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func rvaToolUse(id, input string) []api.StreamEvent {
	return []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: 50},
		{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "tool_use", Index: 0, ID: id, Name: "structured_output"}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "input_json_delta", PartialJSON: input}},
		{Type: api.EventContentBlockStop, Index: 0},
		{Type: api.EventMessageDelta, StopReason: "tool_use", Usage: api.UsageDelta{Reported: true, OutputTokens: 20}},
		{Type: api.EventMessageStop},
	}
}

func rvaText(text string) []api.StreamEvent {
	return []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: 50},
		{Type: api.EventContentBlockStart, ContentBlock: api.ContentBlockInfo{Type: "text", Index: 0}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "text_delta", Text: text}},
		{Type: api.EventContentBlockStop, Index: 0},
		{Type: api.EventMessageDelta, StopReason: "end_turn", Usage: api.UsageDelta{Reported: true, OutputTokens: 20}},
		{Type: api.EventMessageStop},
	}
}

func rvaWorkflow(backend string) *ir.Workflow {
	return &ir.Workflow{
		Name:  "rva",
		Entry: "judge",
		Nodes: map[string]ir.Node{
			"judge": &ir.AgentNode{
				BaseNode:     ir.BaseNode{ID: "judge"},
				LLMFields:    ir.LLMFields{Backend: backend, Model: "test/test-model", UserPrompt: "ask"},
				SchemaFields: ir.SchemaFields{OutputSchema: "verdict_schema"},
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges:   []*ir.Edge{{From: "judge", To: "done"}},
		Prompts: map[string]*ir.Prompt{"ask": {Body: rvaPrompt}},
		Schemas: map[string]*ir.Schema{"verdict_schema": {Name: "verdict_schema", Fields: []*ir.SchemaField{
			{Name: "verdict", Type: ir.FieldTypeBool},
			{Name: "reason", Type: ir.FieldTypeString},
		}}},
		Vars:  map[string]*ir.Var{},
		Loops: map[string]*ir.Loop{},
	}
}

func lastUser(req api.CreateMessageRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			var b strings.Builder
			for _, c := range req.Messages[i].Content {
				b.WriteString(c.Text)
			}
			return b.String()
		}
	}
	return ""
}

func dumpRecovery(t *testing.T, st store.RunStore, runID string) {
	t.Helper()
	events, err := st.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type == store.EventNodeRecovery {
			t.Logf("node_recovery: %v", ev.Data)
		}
	}
}

func TestRVA_ClawParseFailureIsRepeatedByGenericRecovery(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	mock := &rvaScriptClient{scripts: [][]api.StreamEvent{
		rvaToolUse("t1", `{"verdict":tru`), rvaToolUse("t2", `{"verdict":tru`), // business call: forced + its internal unforced re-ask
		rvaToolUse("t3", `{"verdict":true,"reason":"ok"}`), // whatever comes next
	}}
	reg := model.NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) { return mock, nil })
	dreg := delegate.NewRegistry()
	dreg.Register(delegate.BackendClaw, model.NewClawBackend(reg, model.EventHooks{}, model.RetryPolicy{MaxAttempts: 1}))
	wf := rvaWorkflow(delegate.BackendClaw)
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exec := model.NewClawExecutor(reg, wf, model.WithBackendRegistry(dreg), model.WithDefaultBackend(delegate.BackendClaw), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
	eng := runtime.New(wf, st, exec, runtime.WithOutputValidation(true), runtime.WithLogger(iterlog.New(iterlog.LevelError, io.Discard)), runtime.WithRecoveryDispatch(Dispatch(DefaultRecipes())))
	runErr := eng.Run(context.Background(), "rva-c", nil)
	calls := mock.calls
	t.Logf("run err=%v ; provider calls=%d", runErr, len(calls))
	for i, c := range calls {
		u := lastUser(c)
		t.Logf("call %d: last user turn has business prompt=%v feedback marker=%v", i+1, strings.Contains(u, rvaPrompt), strings.Contains(u, rvaFeedbackMarker))
	}
	dumpRecovery(t, st, "rva-c")
	if len(calls) >= 3 && strings.Contains(lastUser(calls[2]), rvaPrompt) && !strings.Contains(lastUser(calls[2]), rvaFeedbackMarker) {
		t.Errorf("claw parse failure -> generic recovery repeated the business prompt WITHOUT feedback (call 3)")
	}
}

type rvaProseBackend struct {
	mu    sync.Mutex
	tasks []delegate.Task
}

func (b *rvaProseBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tasks = append(b.tasks, task)
	return delegate.Result{Output: map[string]any{"text": "The verdict is pass, because the diff is fine."}, ParseFallback: true,
		Tokens: 10, CostUSD: 0.01, SessionID: fmt.Sprintf("s%d", len(b.tasks)), BackendName: delegate.BackendClaudeCode}, nil
}

func TestRVA_ExtractionFailureIsRepeatedByGenericRecovery(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	var scripts [][]api.StreamEvent
	for i := 0; i < 20; i++ {
		scripts = append(scripts, rvaText("I would rather narrate."))
	}
	extraction := &rvaScriptClient{scripts: scripts}
	reg := model.NewRegistry()
	reg.Register("anthropic", func(string) (api.APIClient, error) { return extraction, nil })
	prose := &rvaProseBackend{}
	dreg := delegate.NewRegistry()
	dreg.Register(delegate.BackendClaudeCode, prose)
	wf := rvaWorkflow(delegate.BackendClaudeCode)
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exec := model.NewClawExecutor(reg, wf, model.WithBackendRegistry(dreg), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
	eng := runtime.New(wf, st, exec, runtime.WithOutputValidation(true), runtime.WithLogger(iterlog.New(iterlog.LevelError, io.Discard)), runtime.WithRecoveryDispatch(Dispatch(DefaultRecipes())))
	runErr := eng.Run(context.Background(), "rva-d", nil)
	t.Logf("run err=%v", runErr)
	t.Logf("business-backend delegations=%d, extraction provider calls=%d", len(prose.tasks), len(extraction.calls))
	repeats := 0
	for i, task := range prose.tasks {
		fb := strings.Contains(task.UserPrompt, rvaFeedbackMarker)
		bp := strings.Contains(task.UserPrompt, rvaPrompt)
		t.Logf("delegation %d: session=%q business prompt=%v feedback=%v", i+1, task.SessionID, bp, fb)
		if i > 0 && bp && !fb {
			repeats++
		}
	}
	dumpRecovery(t, st, "rva-d")
	run, _ := st.LoadRun(context.Background(), "rva-d")
	if run != nil {
		t.Logf("ledger=%+v", run.OutputCorrections)
	}
	if repeats > 0 {
		t.Errorf("%d generic repeat(s) of the business prompt without feedback after the extraction failed", repeats)
	}
}

type rvaFailingClient struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (c *rvaFailingClient) StreamResponse(context.Context, api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return nil, c.err
}

func TestRVA_ExtractionAuthFailureIsBlamedOnTheNode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	extraction := &rvaFailingClient{err: &api.APIError{StatusCode: 401, Message: "invalid x-api-key"}}
	reg := model.NewRegistry()
	reg.Register("anthropic", func(string) (api.APIClient, error) { return extraction, nil })
	prose := &rvaProseBackend{}
	dreg := delegate.NewRegistry()
	dreg.Register(delegate.BackendClaudeCode, prose)
	wf := rvaWorkflow(delegate.BackendClaudeCode)
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exec := model.NewClawExecutor(reg, wf, model.WithBackendRegistry(dreg), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
	eng := runtime.New(wf, st, exec, runtime.WithOutputValidation(true), runtime.WithLogger(iterlog.New(iterlog.LevelError, io.Discard)), runtime.WithRecoveryDispatch(Dispatch(DefaultRecipes())))
	runErr := eng.Run(context.Background(), "rva-j", nil)
	run, _ := st.LoadRun(context.Background(), "rva-j")
	t.Logf("run err=%v", runErr)
	if run != nil {
		t.Logf("status=%s failure_code=%s", run.Status, run.FailureCode)
	}
	t.Logf("business delegations=%d extraction calls=%d", len(prose.tasks), extraction.calls)
	dumpRecovery(t, st, "rva-j")
	var schemaErr *model.OutputSchemaError
	if run == nil || run.FailureCode != runtime.ErrCodeSchemaValidation || !errors.As(runErr, &schemaErr) {
		t.Fatalf("extraction credential must not replace node failure: run=%+v err=%v", run, runErr)
	}
	if len(prose.tasks) != 2 || extraction.calls != 1 {
		t.Fatalf("delegations=%d extraction=%d", len(prose.tasks), extraction.calls)
	}
}

func TestExecutorSchemaFailureUsesProductionRecovery(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv(model.EnvOutputCorrectionBudget, "0")
	client := &rvaScriptClient{scripts: [][]api.StreamEvent{rvaToolUse("invalid", `{"verdict":true}`)}}
	reg := model.NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) { return client, nil })
	dreg := delegate.NewRegistry()
	dreg.Register(delegate.BackendClaw, model.NewClawBackend(reg, model.EventHooks{}, model.RetryPolicy{MaxAttempts: 1}))
	wf := rvaWorkflow(delegate.BackendClaw)
	exec := model.NewClawExecutor(reg, wf, model.WithBackendRegistry(dreg))
	_, err := exec.Execute(context.Background(), wf.Nodes["judge"], nil)
	action, code := Dispatch(DefaultRecipes())(context.Background(), err, func(runtime.ErrorCode) int { return 0 })
	if code != runtime.ErrCodeSchemaValidation || action.Kind != runtime.RecoveryFailTerminal {
		t.Fatalf("real executor schema failure repeats business task: code=%s action=%v err=%v", code, action.Kind, err)
	}
}

func TestClawAndClaudeSchemaExhaustionHaveSameLifecycle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	var firstPhases string
	for _, name := range []string{delegate.BackendClaw, delegate.BackendClaudeCode} {
		t.Run(name, func(t *testing.T) {
			reg := model.NewRegistry()
			mock := &rvaScriptClient{scripts: [][]api.StreamEvent{rvaToolUse("a", `{"verdict":tru`), rvaToolUse("b", `{"verdict":tru`), rvaToolUse("c", `{"verdict":fals`), rvaToolUse("d", `{"verdict":fals`)}}
			reg.Register("test", func(string) (api.APIClient, error) { return mock, nil })
			reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
			backends := delegate.NewRegistry()
			prose := &rvaProseBackend{}
			if name == delegate.BackendClaw {
				backends.Register(name, model.NewClawBackend(reg, model.EventHooks{}, model.RetryPolicy{MaxAttempts: 1}))
			} else {
				backends.Register(name, prose)
			}
			wf := rvaWorkflow(name)
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			retries := 0
			exec := model.NewClawExecutor(reg, wf, model.WithBackendRegistry(backends), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}), model.WithEventHooks(model.EventHooks{OnDelegateRetry: func(_ string, info model.DelegateInfo) {
				if info.Reask != "" {
					retries++
				}
			}}))
			eng := runtime.New(wf, st, exec, runtime.WithOutputCorrectionBudget(1), runtime.WithRecoveryDispatch(Dispatch(DefaultRecipes())))
			runErr := eng.Run(context.Background(), "schema-exhaustion", nil)
			var typed *model.OutputSchemaError
			if !errors.As(runErr, &typed) || Classify(runErr) != runtime.ErrCodeSchemaValidation || retries != 1 {
				t.Fatalf("schema exhaustion bypasses correction: err=%v retries=%d", runErr, retries)
			}
			events, err := st.LoadEvents(context.Background(), "schema-exhaustion")
			if err != nil {
				t.Fatal(err)
			}
			var phases []string
			for _, ev := range events {
				if ev.Type == store.EventNodeRecovery && ev.Data["strategy"] == "schema_output_correction" {
					phases = append(phases, fmt.Sprint(ev.Data["phase"]))
				}
			}
			got := strings.Join(phases, ",")
			if name == delegate.BackendClaw {
				firstPhases = got
			} else if got != firstPhases {
				t.Fatalf("lifecycle differs: claw=%s claude_code=%s", firstPhases, got)
			}
			if len(phases) < 2 {
				t.Fatalf("missing correction lifecycle: %v", phases)
			}
			if name == delegate.BackendClaw && (len(mock.calls) != 4 || !strings.Contains(lastUser(mock.calls[2]), "malformed JSON")) {
				t.Fatalf("parse feedback missing: calls=%d", len(mock.calls))
			}
		})
	}
}
