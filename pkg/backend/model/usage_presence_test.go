package model

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func aggregateEvents(t *testing.T, ctx context.Context, events []api.StreamEvent, closeCh bool) aggregatedResponse {
	t.Helper()
	ch := make(chan api.StreamEvent, len(events)+1)
	for _, ev := range events {
		ch <- ev
	}
	if closeCh {
		close(ch)
	}
	return aggregateStream(ctx, ch)
}

var textHead = []api.StreamEvent{
	{Type: api.EventMessageStart, InputTokens: 100},
	{Type: api.EventContentBlockStart, ContentBlock: api.ContentBlockInfo{Type: "text", Index: 0}},
	{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "text_delta", Text: "Hel"}},
}

func withHead(tail ...api.StreamEvent) []api.StreamEvent {
	return append(append([]api.StreamEvent(nil), textHead...), tail...)
}

// A call's usage counts as reported only when the provider reported it —
// on its message_delta, or on the failure frame that ended it. Every other
// ending leaves the call unreported, its counters a lower bound.
func TestAggregateStream_UsageIsReportedOnlyWhenTheProviderSaysSo(t *testing.T) {
	stop := api.StreamEvent{Type: api.EventContentBlockStop, Index: 0}
	cases := []struct {
		name       string
		events     []api.StreamEvent
		unreported int
		input      int
		output     int
	}{
		{"reported message_delta", withHead(stop,
			api.StreamEvent{Type: api.EventMessageDelta, StopReason: "end_turn", Usage: api.UsageDelta{Reported: true, OutputTokens: 7}},
			api.StreamEvent{Type: api.EventMessageStop}), 0, 100, 7},
		{"message_delta with no usage", withHead(stop,
			api.StreamEvent{Type: api.EventMessageDelta, StopReason: "end_turn"},
			api.StreamEvent{Type: api.EventMessageStop}), 1, 100, 0},
		{"stream cut before message_delta", withHead(), 1, 100, 0},
		{"failure frame carrying the provider's usage", withHead(
			api.StreamEvent{Type: api.EventError, ErrorMessage: "openai response incomplete: max_output_tokens", Usage: api.UsageDelta{Reported: true, InputTokens: 120, OutputTokens: 64}}), 0, 120, 64},
		{"failure frame with what the stream reported", withHead(
			api.StreamEvent{Type: api.EventError, ErrorMessage: "openai stream truncated: closed without finish_reason or [DONE]", Usage: api.UsageDelta{OutputTokens: 9}}), 1, 100, 9},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agg := aggregateEvents(t, context.Background(), tc.events, true)
			if u := agg.usage; u.UnreportedCalls != tc.unreported || u.InputTokens != tc.input || u.OutputTokens != tc.output {
				t.Errorf("usage = %+v, want unreported %d, input %d, output %d", u, tc.unreported, tc.input, tc.output)
			}
		})
	}
	t.Run("cancelled mid-stream", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if agg := aggregateEvents(t, ctx, withHead(), false); agg.usage.UnreportedCalls != 1 {
			t.Errorf("usage = %+v, want the cancelled call unreported", agg.usage)
		}
	})
}

// A call the provider served before failing was billed: its partial usage,
// and whether it went unreported, reach the total the failure path meters.
func TestGenerateTextDirect_AFailedCallKeepsItsUsage(t *testing.T) {
	client := newMockClient(withHead(api.StreamEvent{Type: api.EventError, ErrorMessage: "openai stream truncated: closed without finish_reason or [DONE]", Usage: api.UsageDelta{OutputTokens: 9}}))
	res, err := GenerateTextDirect(context.Background(), client, GenerationOptions{Model: "openai/gpt-x",
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}}})
	if err == nil {
		t.Fatal("the failed call succeeded")
	}
	if u := res.TotalUsage; u.InputTokens != 100 || u.OutputTokens != 9 || u.UnreportedCalls != 1 {
		t.Errorf("TotalUsage = %+v, want the failed call's 100/9 tokens, unreported", u)
	}
}

// A failure whose only spend is a call that went unreported is a bill of
// unknown size, not nothing: the metered output says so.
func TestMeteredFailure_AnUnreportedCallIsABill(t *testing.T) {
	res := meteredFailure(delegate.Task{Model: "openai/gpt-x"}, Usage{UnreportedCalls: 1})
	if got := cost.UnreportedCalls(res.Output); got != 1 {
		t.Errorf("metered output %v, want one unreported call", res.Output)
	}
	if res := meteredFailure(delegate.Task{Model: "openai/gpt-x"}, Usage{}); res.Output != nil {
		t.Errorf("a spendless failure metered %v, want nothing", res.Output)
	}
}

// A human-node generation that failed after an unreported call is a bill too.
func TestMeteredHumanFailure_AnUnreportedCallIsABill(t *testing.T) {
	out := meteredHumanFailure("openai/gpt-x", &ObjectResult[map[string]any]{TotalUsage: Usage{UnreportedCalls: 1}})
	if cost.UnreportedCalls(out) != 1 {
		t.Errorf("metered output %v, want one unreported call", out)
	}
}

// The claw backend's outputs name their unreported calls; a reported call
// leaves no key.
func TestClawBackend_StampsUnreportedCalls(t *testing.T) {
	for name, tc := range map[string]struct {
		delta api.UsageDelta
		want  int
	}{
		"reported":   {api.UsageDelta{Reported: true, OutputTokens: 7}, 0},
		"unreported": {api.UsageDelta{OutputTokens: 7}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			client := newMockClient(withHead(api.StreamEvent{Type: api.EventContentBlockStop, Index: 0},
				api.StreamEvent{Type: api.EventMessageDelta, StopReason: "end_turn", Usage: tc.delta},
				api.StreamEvent{Type: api.EventMessageStop}))
			res, err := (&ClawBackend{}).generateText(context.Background(), client, delegate.Task{Model: "openai/gpt-x"}, GenerationOptions{Model: "openai/gpt-x",
				Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}}})
			if err != nil {
				t.Fatalf("generateText: %v", err)
			}
			if got := cost.UnreportedCalls(res.Output); got != tc.want {
				t.Errorf("output %v, want %d unreported call(s)", res.Output, tc.want)
			}
		})
	}
}

// An in-place retry and a fallback chain fold the calls that went
// unreported with the rest of the spend — a marker-only attempt included.
func TestSpendFolds_CarryUnreportedCalls(t *testing.T) {
	unreported := func(n, tokens int) delegate.Result {
		out := map[string]any{}
		cost.SetUnreportedCalls(out, n)
		return delegate.Result{Tokens: tokens, Output: out}
	}
	if got := foldSpend(unreported(1, 0), unreported(0, 50), false); cost.UnreportedCalls(got.Output) != 1 {
		t.Errorf("foldSpend dropped a marker-only attempt: %v", got.Output)
	}
	if got := foldSpend(unreported(1, 10), unreported(2, 50), false); cost.UnreportedCalls(got.Output) != 3 {
		t.Errorf("foldSpend = %v, want 3 unreported calls", got.Output)
	}
	var spent chainSpend
	spent.add(unreported(1, 0))
	if got := spent.applyTo(delegate.Result{}); cost.UnreportedCalls(got.Output) != 1 {
		t.Errorf("chainSpend dropped a marker-only route: %+v", got)
	}
	spent.add(unreported(2, 30))
	if got := spent.applyTo(unreported(1, 5)); cost.UnreportedCalls(got.Output) != 4 {
		t.Errorf("chainSpend = %v, want 4 unreported calls", got.Output)
	}
}

// A claw step whose usage went unreported says so on its event, and the
// sandbox relay carries it across — field by field, so no struct conversion
// would catch a dropped one.
func TestStepEvent_CarriesUsageUnreported(t *testing.T) {
	info := toLLMStepInfo(StepResult{Number: 1, Usage: Usage{UnreportedCalls: 1}})
	if !info.UsageUnreported {
		t.Fatal("the step info dropped the marker")
	}
	var envs []delegate.Envelope
	relay := SandboxRelayHooks(func(env delegate.Envelope) error { envs = append(envs, env); return nil }, nil)
	relay.OnLLMStepFinish("n", info)
	if len(envs) != 1 {
		t.Fatalf("%d envelopes, want one", len(envs))
	}
	var ev delegate.EventData
	if err := json.Unmarshal(envs[0].Data, &ev); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	var got LLMStepInfo
	if _, err := ApplyRelayedEvent(EventHooks{OnLLMStepFinish: func(_ string, s LLMStepInfo) { got = s }}, "n", ev.Type, ev.Payload); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !got.UsageUnreported {
		t.Error("the sandbox relay dropped the marker")
	}
}

// A node rescued by the structured-output recovery keeps the unreported calls
// of the delegation it replaced: the recovered object takes the place of the
// delegation's output map, and the count rides across with its cost. Red
// when the recovery re-attaches the cost alone.
func TestValidateAndRetry_ARescuedNodeKeepsItsUnreportedCalls(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	schema := &ir.Schema{Name: "out_schema", Fields: []*ir.SchemaField{{Name: "answer", Type: ir.FieldTypeString}}}
	schemaJSON, err := SchemaToJSON(schema)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	clawReg := NewRegistry()
	clawReg.Register("anthropic", func(string) (api.APIClient, error) {
		return newMockClient(toolUseEvents("tu_1", "structured_output", `{"answer":"blue"}`, 200, 50)), nil
	})
	unreported := func() map[string]any {
		out := map[string]any{"text": "the answer is blue"}
		cost.SetUnreportedCalls(out, 1)
		return out
	}
	delegateReg := delegate.NewRegistry()
	delegateReg.Register("test_backend", &capturingBackend{results: []delegate.Result{{
		Output: unreported(), Tokens: 500, ParseFallback: true, BackendName: "test_backend",
	}}})
	exec := NewClawExecutor(clawReg,
		&ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{"out_schema": schema}},
		WithBackendRegistry(delegateReg),
		WithRetryPolicy(RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}),
	)
	backend, err := delegateReg.Resolve("test_backend")
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	first := delegate.Result{Output: unreported(), Tokens: 1_000, ParseFallback: true, BackendName: "test_backend"}
	out, err := exec.validateAndRetry(context.Background(), backendFields{id: "answerer", outputSchema: "out_schema"},
		"test_backend", backend, &delegate.Task{OutputSchema: schemaJSON}, first, schema)
	if err != nil {
		t.Fatalf("the recovery was supposed to rescue the node: %v", err)
	}
	if got := cost.UnreportedCalls(out.Output); got != 2 {
		t.Errorf("rescued output %v, want the delegation's and its retry's 2 unreported calls", out.Output)
	}
}

// withoutUsageReport strips the provider's account from a scripted call: its
// message_delta then names no usage, so the call went unreported.
func withoutUsageReport(events []api.StreamEvent) []api.StreamEvent {
	out := append([]api.StreamEvent(nil), events...)
	for i := range out {
		if out[i].Type == api.EventMessageDelta {
			out[i].Usage.Reported = false
		}
	}
	return out
}

// Every exit of the claw backend that carries a generation's usage carries
// whether it went unreported: one case per exit, each red when that exit
// stamps or folds the counters without the count. The call count pins the
// case to the exit it names.
func TestClawBackend_EveryExitCarriesItsUnreportedCalls(t *testing.T) {
	schemaJSON, err := SchemaToJSON(&ir.Schema{Name: "answer", Fields: []*ir.SchemaField{{Name: "answer", Type: ir.FieldTypeString}}})
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	bash := []delegate.ToolDef{{
		Name:        "bash",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Execute:     func(context.Context, json.RawMessage) (string, error) { return "ok", nil },
	}}
	const narration = "I will review the diff."
	type script = []api.StreamEvent
	cases := []struct {
		name    string
		tools   bool
		scripts []script
		wantErr bool
		want    int
	}{
		{"structured answer", false, []script{
			withoutUsageReport(toolUseEvents("tu_1", "structured_output", `{"answer":"blue"}`, 100, 20)),
		}, false, 1},
		{"structured call cut mid-answer", false, []script{withHead()}, true, 1},
		{"structured answer the model malformed", false, []script{
			withoutUsageReport(toolUseEvents("tu_1", "structured_output", `{"answer":`, 100, 20)),
		}, true, 1},
		{"tool loop answering in JSON", true, []script{
			withoutUsageReport(textEvents(`{"answer":"blue"}`, 100, 20)),
		}, false, 1},
		{"tool loop whose JSON names a count of its own", true, []script{
			textEvents(`{"answer":"blue","_usage_unreported_calls":1e300}`, 100, 20),
		}, false, 0},
		{"tool loop rescued by the recovery pass", true, []script{
			toolUseEvents("tu_1", "bash", `{}`, 100, 20),
			withoutUsageReport(textEvents("I reviewed it.", 100, 20)),
			withoutUsageReport(toolUseEvents("tu_2", "structured_output", `{"answer":"blue"}`, 100, 20)),
		}, false, 2},
		{"tool loop whose recovery pass was cut", true, []script{
			toolUseEvents("tu_1", "bash", `{}`, 100, 20),
			withoutUsageReport(textEvents("I reviewed it.", 100, 20)),
			withHead(),
		}, false, 2},
		{"narration nudged into an answer", true, []script{
			withoutUsageReport(textEvents(narration, 100, 20)),
			textEvents(`{"answer":"blue"}`, 100, 20),
		}, false, 1},
		{"nudge cut mid-answer", true, []script{
			textEvents(narration, 100, 20),
			withHead(),
		}, true, 1},
		{"nudge refused for good, then recovered", true, []script{
			textEvents(narration, 100, 20),
			withHead(api.StreamEvent{Type: api.EventError, ErrorMessage: "openai response failed: content_filter: blocked"}),
			toolUseEvents("tu_1", "structured_output", `{"answer":"blue"}`, 100, 20),
		}, false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newMockClient(tc.scripts...)
			reg := NewRegistry()
			reg.Register("test", func(string) (api.APIClient, error) { return client, nil })
			task := delegate.Task{NodeID: "n", Model: "test/test-model", UserPrompt: "q", OutputSchema: schemaJSON}
			if tc.tools {
				task.HasTools, task.ToolDefs, task.ToolMaxSteps = true, bash, 5
			}
			res, err := NewClawBackend(reg, EventHooks{}, RetryPolicy{MaxAttempts: 1, MaxAttemptsTransient: 1}).Execute(context.Background(), task)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want an error: %v", err, tc.wantErr)
			}
			if n := len(client.getCalls()); n != len(tc.scripts) {
				t.Fatalf("%d provider call(s), want %d: the case did not take the exit it names", n, len(tc.scripts))
			}
			if got := cost.UnreportedCalls(res.Output); got != tc.want {
				t.Errorf("output %v carries %d unreported call(s), want %d", res.Output, got, tc.want)
			}
		})
	}
}

// An `interaction: llm` human node's answer carries its unreported calls.
func TestHumanLLM_AnAnswerCarriesItsUnreportedCalls(t *testing.T) {
	reg := NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) {
		return newMockClient(withoutUsageReport(toolUseEvents("tu_1", "structured_output", `{"answer":"blue"}`, 100, 20))), nil
	})
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{"answer_schema": {Name: "answer_schema", Fields: []*ir.SchemaField{{Name: "answer", Type: ir.FieldTypeString}}}},
	}
	node := &ir.HumanNode{
		BaseNode:          ir.BaseNode{ID: "gate"},
		InteractionFields: ir.InteractionFields{Interaction: ir.InteractionLLM},
		Model:             "test/test-model",
		SchemaFields:      ir.SchemaFields{OutputSchema: "answer_schema"},
	}
	out, err := NewClawExecutor(reg, wf).Execute(context.Background(), node, map[string]any{"q": "which db?"})
	if err != nil {
		t.Fatalf("the llm half was supposed to answer: %v", err)
	}
	if got := cost.UnreportedCalls(out); got != 1 {
		t.Errorf("answer %v carries %d unreported call(s), want 1", out, got)
	}
}

// A request the provider refused before serving any of it — named a rate
// limit, an overload or an exhausted balance by its code or type — billed
// nothing, so no usage went unreported. Anything served first, any other
// failure, or a refusal named only in prose still counts.
func TestAggregateStream_ARefusalBeforeServingIsNotUnreported(t *testing.T) {
	placeholder := api.StreamEvent{Type: api.EventMessageStart}
	refusal := func(msg string) api.StreamEvent { return api.StreamEvent{Type: api.EventError, ErrorMessage: msg} }
	const rateLimited = "openai stream error: Rate limit reached for requests (type=requests, code=rate_limit_exceeded)"
	cases := []struct {
		name   string
		events []api.StreamEvent
		want   int
	}{
		{"rate limit before any content", []api.StreamEvent{placeholder, refusal(rateLimited)}, 0},
		{"a gateway's 429 before any content", []api.StreamEvent{placeholder, refusal("openai stream error: litellm.RateLimitError: quota hit (type=None, code=429)")}, 0},
		{"overload in a raw error frame", []api.StreamEvent{placeholder, refusal(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)}, 0},
		{"rate limit after content", withHead(refusal(rateLimited)), 1},
		{"rate limit after a counted prompt", []api.StreamEvent{{Type: api.EventMessageStart, InputTokens: 100}, refusal(rateLimited)}, 1},
		{"a refusal frame carrying what it served", func() []api.StreamEvent {
			f := refusal(rateLimited)
			f.Usage.OutputTokens = 9
			return []api.StreamEvent{placeholder, f}
		}(), 1},
		{"content block started", []api.StreamEvent{placeholder, {Type: api.EventContentBlockStart, ContentBlock: api.ContentBlockInfo{Type: "text", Index: 0}}, refusal(rateLimited)}, 1},
		{"a bare delta", []api.StreamEvent{placeholder, {Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "text_delta", Text: "He"}}, refusal(rateLimited)}, 1},
		{"a terminal delta", []api.StreamEvent{placeholder, {Type: api.EventMessageDelta, StopReason: "end_turn", Usage: api.UsageDelta{OutputTokens: 5}}, refusal(rateLimited)}, 1},
		{"server failure before any content", []api.StreamEvent{placeholder, refusal("openai stream error: upstream failed (type=server_error, code=500)")}, 1},
		{"context overflow in a raw frame", []api.StreamEvent{placeholder, refusal(`{"type":"error","error":{"code":"context_length_exceeded","message":"prompt too long"}}`)}, 0},
		{"a bad key", []api.StreamEvent{placeholder, refusal("openai stream error: Unauthorized (type=authentication_error, code=401)")}, 0},
		{"an unknown model", []api.StreamEvent{placeholder, refusal("openai response failed: model_not_found: no such model")}, 0},
		{"a timeout that a retry may clear", []api.StreamEvent{placeholder, refusal("openai stream error: gateway timeout (type=invalid_request_error, code=408)")}, 1},
		{"refusal named only in prose", []api.StreamEvent{placeholder, refusal("openai stream error: you are being rate limited")}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agg := aggregateEvents(t, context.Background(), tc.events, true)
			if agg.err == nil {
				t.Fatal("the refused call succeeded")
			}
			if agg.usage.UnreportedCalls != tc.want {
				t.Errorf("usage = %+v, want %d unreported call(s)", agg.usage, tc.want)
			}
		})
	}
}

// A failure frame's reasoning count is billed output like its other
// counters, and joins them.
func TestAggregateStream_AFailureFrameKeepsItsReasoningTokens(t *testing.T) {
	frame := api.StreamEvent{Type: api.EventError, ErrorMessage: "openai stream truncated: closed without finish_reason or [DONE]"}
	frame.Usage.OutputTokens = 40
	frame.Usage.OutputTokensDetails.ThinkingTokens = 30
	if agg := aggregateEvents(t, context.Background(), withHead(frame), true); agg.usage.ReasoningTokens != 30 {
		t.Errorf("usage = %+v, want the frame's 30 reasoning tokens", agg.usage)
	}
}

// A request the cold watchdog abandoned unanswered went out: whatever the
// provider spent on it, nothing reported it. A provider that answered with
// an error served nothing.
func TestGenerateTextDirect_ARequestAbandonedUnansweredIsUnreported(t *testing.T) {
	t.Setenv("ITERION_CLAW_STREAM_COLD_TIMEOUT", "20ms")
	opts := GenerationOptions{Model: "test-model", Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}}}
	blocked := &blockingStartupClient{entered: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
	t.Cleanup(func() {
		close(blocked.release)
		<-blocked.done
	})
	res, err := GenerateTextDirect(context.Background(), blocked, opts)
	if !errors.As(err, new(*StreamIdleError)) {
		t.Fatalf("err = %v, want the cold watchdog's", err)
	}
	if res == nil || res.TotalUsage.UnreportedCalls != 1 {
		t.Errorf("result = %+v, want the abandoned request unreported", res)
	}

	res, err = GenerateTextDirect(context.Background(), &execMockClient{err: errors.New("401 unauthorized")}, opts)
	if err == nil {
		t.Fatal("the refused request succeeded")
	}
	if res != nil && res.TotalUsage.UnreportedCalls != 0 {
		t.Errorf("result = %+v, want a refused request to count nothing", res)
	}
}

// A stream that opens LATE inherits what remains of the cold budget: a
// provider granted a second full window could sit silent well past the
// deadline the policy named. Virtual clock, so the 30 ms open and the 50 ms
// budget are exact.
func TestGenerateTextDirect_ALateOpeningStreamInheritsTheColdRemainder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t.Setenv("ITERION_CLAW_STREAM_COLD_TIMEOUT", "50ms")
		start := time.Now()
		res, err := GenerateTextDirect(context.Background(), lateOpenClient{delay: 30 * time.Millisecond}, GenerationOptions{
			Model:    "test-model",
			Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
		})
		elapsed := time.Since(start)
		var idle *StreamIdleError
		if !errors.As(err, &idle) || idle.Phase != StreamIdleCold {
			t.Fatalf("err = %v, want the cold watchdog's", err)
		}
		if res == nil || res.TotalUsage.UnreportedCalls != 1 {
			t.Errorf("result = %+v, want the abandoned request unreported", res)
		}
		// 30 ms to open + the 20 ms that remained of the 50 ms budget. A
		// second full window would answer at 80 ms.
		if elapsed < 45*time.Millisecond || elapsed > 65*time.Millisecond {
			t.Errorf("the idle error came after %v, want the remainder (~50ms), not a second window (80ms)", elapsed)
		}
	})
}

// lateOpenClient grants the stream only after a delay, then stays silent
// forever (its channel closes when the request context does).
type lateOpenClient struct{ delay time.Duration }

func (c lateOpenClient) StreamResponse(ctx context.Context, _ api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	select {
	case <-time.After(c.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	ch := make(chan api.StreamEvent)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}
