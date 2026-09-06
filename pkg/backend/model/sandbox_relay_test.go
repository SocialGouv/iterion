package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// relayHost builds the launcher half of the sandbox IPC over a real run
// store: a ClawBackend whose hooks are the production store hooks, and a
// loader for what those hooks persisted. The store is the oracle — the
// events the studio, the report and the runner's metering read.
func relayHost(t *testing.T, runID string) (*ClawBackend, func() []*store.Event) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	hooks := NewStoreEventHooks(ctx, st, runID, iterlog.Nop(), nil)
	b := NewClawBackend(NewRegistry(), hooks, RetryPolicy{})
	return b, func() []*store.Event {
		evts, err := st.LoadEvents(ctx, runID)
		if err != nil {
			t.Fatalf("LoadEvents: %v", err)
		}
		return evts
	}
}

func eventsOfType(evts []*store.Event, typ store.EventType) []*store.Event {
	var out []*store.Event
	for _, e := range evts {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// #805 — the relay boundary, end to end on the real wire: the runner-side
// hooks encode llm_request / llm_step_finished as `event` envelopes, a
// real Multiplexer carries them to the launcher's handler, and the
// launcher's own store hooks persist them — with the same payload the
// in-process path writes, plus what those hooks derive from it (the
// assistant_text narration of a tool-bearing step, the usage_progress
// sample a supervisor's cost_gt monitor arms on). Before the relay, a
// sandboxed claw node left none of these on the host.
func TestSandboxRelay_StepEventsCrossTheWireIntoTheHostStore(t *testing.T) {
	b, load := relayHost(t, "run-relay")
	handler := b.multiplexerHandler(context.Background(), delegate.Task{NodeID: "plan_review", Model: "openai/gpt-5.6-sol"})
	if handler.OnEvent == nil {
		t.Fatal("the launcher's multiplexer handler has no OnEvent: every event envelope the runner emits is dropped")
	}

	// Only the runner→launcher direction carries traffic here: no tool_call
	// or ask_user asks for a reply, so the launcher→runner pipe is never
	// written and needs no reader.
	_, runnerStdinW := io.Pipe()
	runnerStdoutR, runnerStdoutW := io.Pipe()
	mux := delegate.NewMultiplexer(runnerStdoutR, runnerStdinW, handler)

	// The runner half: the hooks the in-container claw backend fires,
	// writing on its IPC stdout.
	var relayErrs []error
	go func() {
		defer runnerStdoutW.Close()
		writer := delegate.NewEnvelopeWriter(runnerStdoutW)
		relay := SandboxRelayHooks(writer.Write, func(err error) { relayErrs = append(relayErrs, err) })
		relay.OnLLMRequest("plan_review", LLMRequestInfo{
			Model: "gpt-5.6-sol", MessageCount: 3, ToolCount: 4, ReasoningEffort: "high", Iteration: 2, Timestamp: time.Now(),
		})
		relay.OnLLMStepFinish("plan_review", LLMStepInfo{
			Number:           1,
			Text:             "Reading the plan before I judge it.",
			ToolCalls:        []ToolCallEntry{{Name: "read_file", Input: json.RawMessage(`{"path":"PLAN.md"}`)}},
			FinishReason:     "tool_use",
			InputTokens:      20000,
			OutputTokens:     300,
			CacheReadTokens:  1500,
			CacheWriteTokens: 250,
			ReasoningTokens:  120,
			ThinkingMs:       900,
			Thinking:         "The plan skips the migration.",
			Iteration:        2,
		})
		resEnv, _ := delegate.NewResultEnvelope(delegate.IOResult{Tokens: 20300})
		_ = writer.Write(resEnv)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := mux.Run(ctx); err != nil {
		t.Fatalf("multiplexer: %v", err)
	}
	if len(relayErrs) != 0 {
		t.Fatalf("relay reported %v", relayErrs)
	}

	evts := load()
	reqs := eventsOfType(evts, store.EventLLMRequest)
	if len(reqs) != 1 {
		t.Fatalf("llm_request events = %d, want 1 (events: %+v)", len(reqs), evts)
	}
	req := reqs[0]
	if req.NodeID != "plan_review" {
		t.Errorf("llm_request node = %q, want the launcher's task node", req.NodeID)
	}
	if req.Data["model"] != "gpt-5.6-sol" || req.Data["message_count"] != float64(3) ||
		req.Data["tool_count"] != float64(4) || req.Data["reasoning_effort"] != "high" {
		t.Errorf("llm_request data = %v, want model/message_count/tool_count/reasoning_effort intact", req.Data)
	}

	steps := eventsOfType(evts, store.EventLLMStepFinished)
	if len(steps) != 1 {
		t.Fatalf("llm_step_finished events = %d, want 1", len(steps))
	}
	step := steps[0]
	want := map[string]any{
		"step":               float64(1),
		"input_tokens":       float64(20000),
		"output_tokens":      float64(300),
		"cache_read_tokens":  float64(1500),
		"cache_write_tokens": float64(250),
		"thinking_tokens":    float64(120),
		"thinking_ms":        float64(900),
		"finish_reason":      "tool_use",
		"tool_calls":         float64(1),
		"response_text":      "Reading the plan before I judge it.",
	}
	for k, v := range want {
		if step.Data[k] != v {
			t.Errorf("llm_step_finished %s = %v, want %v", k, step.Data[k], v)
		}
	}
	if _, present := step.Data["thinking"]; present {
		t.Error("thinking text persisted on the event — the in-process hook keeps it out of events.jsonl")
	}

	// Derived by the host hooks from the relayed step, exactly as in-process.
	if narr := eventsOfType(evts, store.EventAssistantText); len(narr) != 1 || narr[0].Data["text"] != "Reading the plan before I judge it." {
		t.Errorf("assistant_text events = %+v, want the tool-bearing step's narration", narr)
	}
	ups := eventsOfType(evts, store.EventUsageProgress)
	if len(ups) != 1 {
		t.Fatalf("usage_progress events = %d, want 1 — a 22k-token step is past the sampling floor", len(ups))
	}
	if ups[0].Data["tokens"] != float64(20000+300+1500+250) || ups[0].Data["model"] != "gpt-5.6-sol" {
		t.Errorf("usage_progress data = %v, want the step's tokens priced on the relayed model", ups[0].Data)
	}
}

// A payload a newer runner may relay and this host does not consume is
// dropped, never fatal; a payload of a known type that does not decode is
// an error the caller must surface — the node's metering is incomplete.
func TestApplyRelayedEvent_UnknownIsDroppedMalformedIsAnError(t *testing.T) {
	var fired int
	hooks := EventHooks{OnLLMStepFinish: func(string, LLMStepInfo) { fired++ }}

	handled, err := ApplyRelayedEvent(hooks, "n", "tool_called", map[string]any{"name": "bash"})
	if handled || err != nil {
		t.Fatalf("unknown type: handled=%v err=%v, want dropped without error", handled, err)
	}
	handled, err = ApplyRelayedEvent(hooks, "n", string(store.EventLLMStepFinished), map[string]any{"input_tokens": "twelve"})
	if !handled || err == nil {
		t.Fatalf("malformed known type: handled=%v err=%v, want an error", handled, err)
	}
	if fired != 0 {
		t.Fatalf("hook fired %d time(s) on a malformed payload", fired)
	}
}

// The launcher-side handler surfaces a decode failure instead of dropping
// it: the run log names the node and the event type.
func TestSandboxRelay_HostWarnsOnAnUndecodableRelayedEvent(t *testing.T) {
	var logBuf strings.Builder
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{}, WithClawLogger(iterlog.New(iterlog.LevelWarn, &logBuf)))
	handler := b.multiplexerHandler(context.Background(), delegate.Task{NodeID: "plan_review"})
	handler.OnEvent(string(store.EventLLMRequest), map[string]any{"model": 42})
	if !strings.Contains(logBuf.String(), "plan_review") || !strings.Contains(logBuf.String(), "llm_request") {
		t.Fatalf("no warning naming the node and event type, log: %q", logBuf.String())
	}
}

// A write that fails is reported, not swallowed: the runner cannot return
// an error from a hook, and a dead channel must leave a trace.
func TestSandboxRelayHooks_ReportsAFailedWrite(t *testing.T) {
	var reported []error
	hooks := SandboxRelayHooks(
		func(delegate.Envelope) error { return errors.New("broken pipe") },
		func(err error) { reported = append(reported, err) },
	)
	hooks.OnLLMRequest("n", LLMRequestInfo{Model: "m"})
	if len(reported) != 1 || !strings.Contains(reported[0].Error(), "llm_request") || !strings.Contains(reported[0].Error(), "broken pipe") {
		t.Fatalf("reported = %v, want one error naming the event and the cause", reported)
	}
}
