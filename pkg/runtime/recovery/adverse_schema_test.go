package recovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/errtrack"
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
	st, storeErr := store.New(t.TempDir())
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	err := runtime.New(wf, st, exec, runtime.WithRecoveryDispatch(Dispatch(DefaultRecipes()))).Run(context.Background(), "default-dispatch", nil)
	action, code := Dispatch(DefaultRecipes())(context.Background(), err, func(runtime.ErrorCode) int { return 0 })
	if code != runtime.ErrCodeSchemaValidation || action.Kind != runtime.RecoveryFailTerminal {
		t.Fatalf("real executor schema failure repeats business task: code=%s action=%v err=%v", code, action.Kind, err)
	}
}

func TestClawAndClaudeSchemaExhaustionHaveSameLifecycle(t *testing.T) {
	t.Setenv(model.EnvOutputCorrectionBudget, "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	for _, name := range []string{delegate.BackendClaw, delegate.BackendClaudeCode} {
		t.Run(name, func(t *testing.T) {
			reg := model.NewRegistry()
			mock := &rvaScriptClient{scripts: [][]api.StreamEvent{rvaToolUse("a", `{"verdict":tru`), rvaToolUse("b", `{"verdict":tru`), rvaToolUse("c", `{"verdict":tru`), rvaToolUse("d", `{"verdict":tru`)}}
			reg.Register("test", func(string) (api.APIClient, error) { return mock, nil })
			extraction := &rvaScriptClient{scripts: [][]api.StreamEvent{rvaToolUse("ex", `{"verdict":true,"reason":"fabricated from diagnostics"}`)}}
			reg.Register("anthropic", func(string) (api.APIClient, error) {
				if name == delegate.BackendClaw {
					return extraction, nil
				}
				return mock, nil
			})
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
			eng := runtime.New(wf, st, exec, runtime.WithRecoveryDispatch(Dispatch(DefaultRecipes())))
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
			if len(phases) < 2 {
				t.Fatalf("missing correction lifecycle: %v", phases)
			}
			// Claw's malformed tool block has no prose to extract. Claude Code
			// supplies prose, so its additional extraction call shares the same
			// allowance: one re-ask + one extraction, never two business retries.
			// Owned events report committed transitions, not transient callback states.
			want := "attempt,unchanged"
			wantAttempts := 1
			if name == delegate.BackendClaudeCode {
				want = "attempt,attempt,unchanged"
				wantAttempts = 2
				if len(prose.tasks) != 2 || len(mock.calls) != 2 {
					t.Errorf("claude calls: business/reask=%d extraction=%d", len(prose.tasks), len(mock.calls))
				}
			} else if len(extraction.calls) != 0 {
				t.Errorf("diagnostic extraction calls=%d", len(extraction.calls))
			}
			if got != want {
				t.Fatalf("%s lifecycle=%s want=%s", name, got, want)
			}
			attempts := 0
			for _, phase := range phases {
				if phase == "attempt" {
					attempts++
				}
			}
			if attempts != wantAttempts {
				t.Fatalf("%s attempts=%d want=%d", name, attempts, wantAttempts)
			}

			if name == delegate.BackendClaw && (len(mock.calls) != 4 || !strings.Contains(lastUser(mock.calls[2]), "malformed JSON")) {
				t.Fatalf("parse feedback missing: calls=%d", len(mock.calls))
			}
		})
	}
}

type ownedExtractionAmbiguous struct{}

func (ownedExtractionAmbiguous) Error() string         { return "auxiliary transport ambiguous" }
func (ownedExtractionAmbiguous) AmbiguousEffect() bool { return true }
func TestOwnedExtractionAllowanceAndControl(t *testing.T) {
	// parent-cancel-unwatched starts the stream without the cold watchdog:
	// the provider's ambiguous failure is returned after the cancellation with
	// no select between them, the order parent-cancel reaches only when the
	// scheduler picks the provider's arm.
	for _, mode := range []string{"zero", "one", "two", "ambiguous", "joined-cancel", "cancel-only", "deadline", "parent-cancel", "parent-cancel-unwatched"} {
		t.Run(mode, func(t *testing.T) {
			parentCancel := mode == "parent-cancel" || mode == "parent-cancel-unwatched"
			if mode == "parent-cancel-unwatched" {
				t.Setenv("ITERION_CLAW_STREAM_COLD_TIMEOUT", "0")
			}
			t.Setenv("HOME", t.TempDir())
			t.Setenv("TMPDIR", t.TempDir())
			t.Setenv("ANTHROPIC_BASE_URL", "")
			t.Setenv("ANTHROPIC_API_KEY", "test-key")
			invoke, cancel := context.WithCancel(context.Background())
			defer cancel()
			budget := 2
			if mode == "zero" {
				budget = 0
			}
			if mode == "one" {
				budget = 1
			}
			t.Setenv(model.EnvOutputCorrectionBudget, fmt.Sprint(budget))
			extract := &rvaScriptClient{scripts: [][]api.StreamEvent{rvaToolUse("ex", `{"verdict":true,"reason":"ok"}`)}}
			failure := &rvaFailingClient{err: ownedExtractionAmbiguous{}}
			if mode == "joined-cancel" {
				failure.err = errors.Join(ownedExtractionAmbiguous{}, context.Canceled)
			}
			if mode == "cancel-only" {
				failure.err = context.Canceled
			}
			if mode == "deadline" {
				failure.err = &model.ErrNodeDeadline{NodeID: "judge", Timeout: time.Second}
			}
			reg := model.NewRegistry()
			reg.Register("anthropic", func(string) (api.APIClient, error) {
				if parentCancel {
					return &ownedExtractionCancelClient{inner: failure, cancel: cancel}, nil
				}
				if mode == "ambiguous" || mode == "joined-cancel" || mode == "cancel-only" || mode == "deadline" {
					return failure, nil
				}
				return extract, nil
			})
			prose := &rvaProseBackend{}
			backends := delegate.NewRegistry()
			backends.Register(delegate.BackendClaudeCode, prose)
			wf := rvaWorkflow(delegate.BackendClaudeCode)
			st, e := store.New(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			exec := model.NewClawExecutor(reg, wf, model.WithBackendRegistry(backends), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
			err := runtime.New(wf, st, exec, runtime.WithRecoveryDispatch(Dispatch(DefaultRecipes()))).Run(invoke, "ownedExtraction", nil)
			r, e := st.LoadRun(context.Background(), "ownedExtraction")
			if e != nil {
				t.Fatal(e)
			}
			ep := r.OutputCorrections["judge"]
			t.Logf("mode=%s business=%d extraction=%d failedExtraction=%d attempts=%d status=%s err=%v", mode, len(prose.tasks), len(extract.calls), failure.calls, ep.Attempts, ep.Status, err)
			if mode == "ambiguous" || mode == "joined-cancel" {
				if !errtrack.IsAmbiguousEffect(err) || ep.Cause == nil || ep.Cause.Kind != "ambiguous" {
					t.Fatalf("AUX_STOP_ERASED err=%v cause=%+v", err, ep.Cause)
				}
				return
			}
			if parentCancel {
				var schema *model.OutputSchemaError
				if err == nil || r.Status != store.RunStatusCancelled || errors.As(err, &schema) {
					t.Fatalf("PARENT_CANCEL_RECLASSIFIED %v status=%s", err, r.Status)
				}
				return
			}
			if mode == "cancel-only" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("AUX_CANCEL_ERASED %v", err)
				}
				return
			}
			if mode == "deadline" {
				var stop *model.ErrNodeDeadline
				if !errors.As(err, &stop) || ep.Cause == nil || ep.Cause.Kind != "node_deadline" {
					t.Fatalf("AUX_DEADLINE_ERASED %v %+v", err, ep.Cause)
				}
				return
			}
			wantBusiness := 2
			if budget == 0 {
				wantBusiness = 1
			}
			wantExtraction := 0
			if budget == 2 {
				wantExtraction = 1
			}
			if len(prose.tasks) != wantBusiness || len(extract.calls) != wantExtraction || ep.Attempts != budget {
				t.Fatalf("allowance counts mismatch")
			}
			if budget == 2 {
				if err != nil || len(ep.Reservations) != 2 || ep.Reservations[0].Receipt.Usage.Tokens != 10 || ep.Reservations[1].Receipt.Usage.Tokens != 70 {
					t.Fatalf("split bills lost: err=%v episode=%+v", err, ep)
				}
			} else {
				var typed *model.OutputSchemaError
				if !errors.As(err, &typed) {
					t.Fatalf("schema disposition lost %v", err)
				}
			}
		})
	}
}

type ownedExtractionStopStore struct {
	*store.FilesystemRunStore
	cancel  context.CancelFunc
	stopped bool
}

func (s *ownedExtractionStopStore) SetRunOutputCorrectionIf(ctx context.Context, id string, a store.RunExecutionAuthority, key string, expected store.OutputCorrectionExpected, next store.OutputCorrectionEpisode) (bool, error) {
	ok, err := s.FilesystemRunStore.SetRunOutputCorrectionIf(ctx, id, a, key, expected, next)
	if ok && err == nil && !s.stopped && next.Attempts == 1 && next.Status == "active" && len(next.Reservations) == 1 && next.Reservations[0].Receipt != nil {
		s.stopped = true
		s.cancel()
	}
	return ok, err
}
func TestOwnedExtractionRestartPreservesAllowance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv(model.EnvOutputCorrectionBudget, "2")
	extract := &rvaScriptClient{scripts: [][]api.StreamEvent{rvaToolUse("ex", `{"verdict":true,"reason":"ok"}`)}}
	reg := model.NewRegistry()
	reg.Register("anthropic", func(string) (api.APIClient, error) { return extract, nil })
	prose := &rvaProseBackend{}
	backends := delegate.NewRegistry()
	backends.Register(delegate.BackendClaudeCode, prose)
	wf := rvaWorkflow(delegate.BackendClaudeCode)
	base, e := store.New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := &ownedExtractionStopStore{FilesystemRunStore: base, cancel: cancel}
	makeEngine := func() *runtime.Engine {
		exec := model.NewClawExecutor(reg, wf, model.WithBackendRegistry(backends), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
		return runtime.New(wf, st, exec, runtime.WithRecoveryDispatch(Dispatch(DefaultRecipes())))
	}
	first := makeEngine().Run(ctx, "ownedExtraction-restart", nil)
	r, e := base.LoadRun(context.Background(), "ownedExtraction-restart")
	if e != nil {
		t.Fatal(e)
	}
	t.Logf("interrupted=%v status=%s attempts=%d delegations=%d extraction=%d", first, r.Status, r.OutputCorrections["judge"].Attempts, len(prose.tasks), len(extract.calls))
	if !st.stopped || len(prose.tasks) != 2 || len(extract.calls) != 0 || r.OutputCorrections["judge"].Attempts != 1 {
		t.Fatal("interruption did not land between operations")
	}
	second := makeEngine().Resume(context.Background(), "ownedExtraction-restart", nil)
	r, e = base.LoadRun(context.Background(), "ownedExtraction-restart")
	if e != nil {
		t.Fatal(e)
	}
	t.Logf("resumed=%v status=%s attempts=%d delegations=%d extraction=%d", second, r.Status, r.OutputCorrections["judge"].Attempts, len(prose.tasks), len(extract.calls))
	if r.OutputCorrections["judge"].Attempts > 2 || len(extract.calls) > 1 {
		t.Fatal("restart exceeded allowance")
	}
}

func (s *ownedExtractionStopStore) SetRunOutputCorrection(ctx context.Context, id, key string, next store.OutputCorrectionEpisode) error {
	err := s.FilesystemRunStore.SetRunOutputCorrection(ctx, id, key, next)
	if err == nil && !s.stopped && next.Attempts == 1 && next.Status == "active" && next.LastOutputFingerprint != "" {
		s.stopped = true
		s.cancel()
	}
	return err
}

type ownedExtractionCancelClient struct {
	inner  *rvaFailingClient
	cancel context.CancelFunc
}

func (c *ownedExtractionCancelClient) StreamResponse(ctx context.Context, r api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	c.cancel()
	return c.inner.StreamResponse(ctx, r)
}

type ownedAuxiliaryOpaque struct{ err error }

func (e ownedAuxiliaryOpaque) Error() string      { return "redacted auxiliary failure" }
func (e ownedAuxiliaryOpaque) As(target any) bool { return errors.As(e.err, target) }
func TestOwnedAuxiliaryStopsPreserveProvenance(t *testing.T) {
	auth := &delegate.ErrAuthFailed{Provider: "auxiliary", Detail: "fixture"}
	window := &delegate.ErrRateLimited{Provider: "auxiliary", Kind: delegate.RateLimitKindUsageWindow, Detail: "fixture"}
	deadline := &model.ErrNodeDeadline{NodeID: "judge", Timeout: time.Second}
	for _, row := range []struct {
		name  string
		cause error
		want  string
	}{
		{"opaque-scalar-cancel", ownedAuxiliaryOpaque{context.Canceled}, "cancel"},
		{"window-cancel", errors.Join(window, context.Canceled), "cancel"},
		{"nested-deadline", &model.ErrNodeDeadline{NodeID: "judge", Timeout: time.Second, Observed: errors.Join(auth, window)}, "node_deadline"},
		{"opaque-nested-deadline", ownedAuxiliaryOpaque{&model.ErrNodeDeadline{NodeID: "judge", Timeout: time.Second, Observed: errors.Join(auth, window)}}, "node_deadline"},
		{"opaque-window-ambiguous", ownedAuxiliaryOpaque{errors.Join(window, ownedExtractionAmbiguous{}, context.Canceled)}, "ambiguous"},
		{"opaque-cancel", ownedAuxiliaryOpaque{errors.Join(errors.New("ordinary"), context.Canceled)}, "cancel"},
		{"opaque-ambiguous", ownedAuxiliaryOpaque{errors.Join(errors.New("ordinary"), ownedExtractionAmbiguous{})}, "ambiguous"},
		{"auth-cancel", errors.Join(auth, context.Canceled), "cancel"},
		{"api-auth-cancel", errors.Join(&api.APIError{StatusCode: 401, Message: "invalid key"}, context.Canceled), "cancel"},
		{"auth-deadline", errors.Join(auth, deadline), "node_deadline"},
		{"opaque-auth-deadline", ownedAuxiliaryOpaque{errors.Join(auth, deadline)}, "node_deadline"},
		{"auth-ambiguous-cancel", errors.Join(auth, ownedExtractionAmbiguous{}, context.Canceled), "ambiguous"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("TMPDIR", t.TempDir())
			t.Setenv("ANTHROPIC_BASE_URL", "")
			t.Setenv("ANTHROPIC_API_KEY", "test-key")
			t.Setenv(model.EnvOutputCorrectionBudget, "2")
			failure := &rvaFailingClient{err: row.cause}
			reg := model.NewRegistry()
			reg.Register("anthropic", func(string) (api.APIClient, error) { return failure, nil })
			prose := &rvaProseBackend{}
			backends := delegate.NewRegistry()
			backends.Register(delegate.BackendClaudeCode, prose)
			wf := rvaWorkflow(delegate.BackendClaudeCode)
			st, e := store.New(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			exec := model.NewClawExecutor(reg, wf, model.WithBackendRegistry(backends), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
			err := runtime.New(wf, st, exec, runtime.WithRecoveryDispatch(Dispatch(DefaultRecipes()))).Run(context.Background(), "r30", nil)
			r, e := st.LoadRun(context.Background(), "r30")
			if e != nil {
				t.Fatal(e)
			}
			ep := r.OutputCorrections["judge"]
			t.Logf("stop=%s business=%d auxiliary=%d attempts=%d status=%s cause=%+v err=%v", row.want, len(prose.tasks), failure.calls, ep.Attempts, r.Status, ep.Cause, err)
			if len(prose.tasks) != 2 || failure.calls != 1 || ep.Attempts != 2 {
				t.Fatal("unexpected paid operation or missing reservation")
			}
			var leakedAuth *delegate.ErrAuthFailed
			var leakedWindow *delegate.ErrRateLimited
			if errors.As(err, &leakedAuth) || errors.As(err, &leakedWindow) {
				t.Fatalf("AUXILIARY_GRAPH_ESCAPED %v", err)
			}
			if row.want == "cancel" {
				var schema *model.OutputSchemaError
				if !errors.As(err, &schema) || !errtrack.AnyCause(err, func(e error) bool { return errors.Is(e, context.Canceled) }) || ep.Cause != nil || !schema.Stopped || strings.Contains(schema.Error(), "budget exhausted") {
					t.Fatalf("AUX_AUTH_LEAK_OR_CANCEL_LOST err=%v cause=%+v", err, ep.Cause)
				}
			} else if ep.Cause == nil || ep.Cause.Kind != row.want {
				t.Fatalf("CONTROL_PRIORITY_LOST want=%s cause=%+v err=%v", row.want, ep.Cause, err)
			}
			if row.want != "cancel" {
				freshExec := model.NewClawExecutor(reg, wf, model.WithBackendRegistry(backends), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
				resumed := runtime.New(wf, st, freshExec, runtime.WithRecoveryDispatch(Dispatch(DefaultRecipes()))).Resume(context.Background(), "r30", nil)
				reread, e := st.LoadRun(context.Background(), "r30")
				if e != nil {
					t.Fatal(e)
				}
				persisted := reread.OutputCorrections["judge"]
				if resumed == nil || errors.As(resumed, &leakedAuth) || errors.As(resumed, &leakedWindow) || persisted.Cause == nil || persisted.Cause.Kind != row.want || persisted.Attempts != 2 || failure.calls != 1 || reread.Status == store.RunStatusPausedWaitingHuman {
					t.Fatalf("REHYDRATED_AUXILIARY_LEAK err=%v cause=%+v calls=%d attempts=%d status=%s", resumed, persisted.Cause, failure.calls, persisted.Attempts, reread.Status)
				}
			}

		})
	}
}

type ownedBusinessAuth struct {
	calls int
	cause error
}

func (b *ownedBusinessAuth) Execute(context.Context, delegate.Task) (delegate.Result, error) {
	b.calls++
	if b.calls > 1 {
		return delegate.Result{}, b.cause
	}
	return delegate.Result{Output: map[string]any{"text": "finished answer"}, ParseFallback: true, SessionID: "business-session", Tokens: 10}, nil
}
func TestOwnedBusinessAuthKeepsProvenance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv(model.EnvOutputCorrectionBudget, "2")
	failure := &rvaFailingClient{err: errors.New("must not extract")}
	reg := model.NewRegistry()
	reg.Register("anthropic", func(string) (api.APIClient, error) { return failure, nil })
	business := &ownedBusinessAuth{cause: &delegate.ErrAuthFailed{Provider: "business", Detail: "fixture"}}
	backends := delegate.NewRegistry()
	backends.Register(delegate.BackendClaudeCode, business)
	wf := rvaWorkflow(delegate.BackendClaudeCode)
	st, e := store.New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	exec := model.NewClawExecutor(reg, wf, model.WithBackendRegistry(backends), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
	err := runtime.New(wf, st, exec, runtime.WithRecoveryDispatch(Dispatch(DefaultRecipes()))).Run(context.Background(), "r32-business", nil)
	r, e := st.LoadRun(context.Background(), "r32-business")
	if e != nil {
		t.Fatal(e)
	}
	ep := r.OutputCorrections["judge"]
	if !errors.Is(err, runtime.ErrRunPaused) || business.calls != 2 || failure.calls != 0 || ep.Cause == nil || ep.Cause.Kind != "auth" || ep.Cause.Provider != "business" {
		t.Fatalf("BUSINESS_PROVENANCE_LOST err=%v business=%d extraction=%d cause=%+v", err, business.calls, failure.calls, ep.Cause)
	}
}
