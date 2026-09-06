package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/store"
)

// relayCarries reports whether the runner-side relay wires the hooks pick
// asks for. Checked before firing, so a hook the relay does not carry
// fails with a message instead of a nil-call panic in the runner
// goroutine.
func relayCarries(pick func(EventHooks) bool) bool {
	return pick(SandboxRelayHooks(func(delegate.Envelope) error { return nil }, nil))
}

// #811 — the tool half of a sandboxed claw node. Its builtins execute
// INSIDE the container, so tool_started / tool_called fire on the runner's
// hooks: without the relay the studio timeline of that node shows LLM
// steps and no tools, the permission gate's denials leave no audit, and a
// supervisor's tool monitor never arms.
func TestSandboxRelay_ToolEventsCrossTheWireIntoTheHostStore(t *testing.T) {
	if !relayCarries(func(h EventHooks) bool { return h.OnToolStarted != nil && h.OnToolCall != nil }) {
		t.Fatal("the runner's relay carries no tool hooks: the in-container builtins are invisible to the host")
	}
	b, load := relayHost(t, "run-relay-tools")
	errs := runRelay(t, b, delegate.Task{NodeID: "campaign"}, func(relay EventHooks) {
		relay.OnToolStarted("campaign", LLMToolStartedInfo{
			ToolName:  "bash",
			ToolUseID: "tu_1",
			InputSize: 42,
			Input:     json.RawMessage(`{"command":"go test ./..."}`),
			Iteration: 3,
		})
		relay.OnToolCall("campaign", LLMToolCallInfo{
			ToolName:  "bash",
			ToolUseID: "tu_1",
			InputSize: 42,
			Output:    "ok\n",
			Duration:  1500 * time.Millisecond,
		})
		relay.OnToolCall("campaign", LLMToolCallInfo{
			ToolName:  "bash",
			ToolUseID: "tu_2",
			InputSize: 20,
			Output:    "Permission denied: Bash(rm:*)",
			Error:     errors.New("Permission denied: Bash(rm:*)"),
		})
	})
	if len(errs) != 0 {
		t.Fatalf("relay reported %v", errs)
	}

	evts := load()
	started := eventsOfType(evts, store.EventToolStarted)
	if len(started) != 1 {
		t.Fatalf("tool_started events = %d, want 1 (events: %+v)", len(started), evts)
	}
	if started[0].NodeID != "campaign" {
		t.Errorf("tool_started node = %q, want the launcher's task node", started[0].NodeID)
	}
	if started[0].Data["tool"] != "bash" || started[0].Data["tool_use_id"] != "tu_1" ||
		started[0].Data["input_size"] != float64(42) || started[0].Data["input"] != `{"command":"go test ./..."}` {
		t.Errorf("tool_started data = %v, want the name, correlation id, size and input intact", started[0].Data)
	}

	called := eventsOfType(evts, store.EventToolCalled)
	if len(called) != 1 {
		t.Fatalf("tool_called events = %d, want 1", len(called))
	}
	if called[0].Data["tool"] != "bash" || called[0].Data["tool_use_id"] != "tu_1" ||
		called[0].Data["duration_ms"] != float64(1500) || called[0].Data["output"] != "ok\n" {
		t.Errorf("tool_called data = %v, want the result and duration intact", called[0].Data)
	}

	// A tool the in-container permission gate refused: the host must
	// persist tool_error carrying the denial, which is the audit.
	failed := eventsOfType(evts, store.EventToolError)
	if len(failed) != 1 {
		t.Fatalf("tool_error events = %d, want 1 — a refused tool leaves no audit otherwise", len(failed))
	}
	if !strings.Contains(fmt.Sprint(failed[0].Data["error"]), "Permission denied") {
		t.Errorf("tool_error data = %v, want the denial reason", failed[0].Data)
	}
}

// #811 — the two loop-health observations of a sandboxed claw node. The
// in-container backend runs its own retry loop and its own compactor, so
// both fire on the runner's hooks; without the relay a run that retried
// three times or compacted its history reads as a silent gap.
func TestSandboxRelay_RetryAndCompactionCrossTheWireIntoTheHostStore(t *testing.T) {
	if !relayCarries(func(h EventHooks) bool { return h.OnLLMRetry != nil && h.OnLLMCompacted != nil }) {
		t.Fatal("the runner's relay carries neither llm_retry nor llm_compacted: the in-container loop's health is invisible")
	}
	b, load := relayHost(t, "run-relay-loop")
	errs := runRelay(t, b, delegate.Task{NodeID: "campaign"}, func(relay EventHooks) {
		relay.OnLLMRetry("campaign", RetryInfo{
			Attempt:    2,
			Error:      errors.New("overloaded_error"),
			StatusCode: 529,
			Delay:      4 * time.Second,
		})
		relay.OnLLMCompacted("campaign", LLMCompactInfo{
			BeforeMessages: 80, AfterMessages: 12, RemovedMessageCount: 68, Iteration: 1,
		})
	})
	if len(errs) != 0 {
		t.Fatalf("relay reported %v", errs)
	}

	evts := load()
	retries := eventsOfType(evts, store.EventLLMRetry)
	if len(retries) != 1 {
		t.Fatalf("llm_retry events = %d, want 1 (events: %+v)", len(retries), evts)
	}
	if retries[0].Data["attempt"] != float64(2) || retries[0].Data["delay_ms"] != float64(4000) ||
		retries[0].Data["status_code"] != float64(529) || retries[0].Data["error"] != "overloaded_error" {
		t.Errorf("llm_retry data = %v, want attempt/delay/status/error intact", retries[0].Data)
	}

	compacts := eventsOfType(evts, store.EventLLMCompacted)
	if len(compacts) != 1 {
		t.Fatalf("llm_compacted events = %d, want 1", len(compacts))
	}
	if compacts[0].Data["before_messages"] != float64(80) || compacts[0].Data["after_messages"] != float64(12) ||
		compacts[0].Data["removed_message_count"] != float64(68) {
		t.Errorf("llm_compacted data = %v, want the message counts intact", compacts[0].Data)
	}
}

// clawTurnConversation is a two-message claw conversation, the shape the
// turn snapshot carries for the Fork API's rehydration.
func clawTurnConversation() []api.Message {
	return []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "ship the feature"}}},
		{Role: "assistant", Content: []api.ContentBlock{
			{Type: "text", Text: "reading the tests first"},
			{Type: "tool_use", ID: "tu_1", Name: "read_file", Input: map[string]any{"path": "main.go"}},
		}},
	}
}

// #811 — the fork anchor of a sandboxed claw node. The turn capture is
// written by the in-container loop; relayed, the host persists the same
// store.TurnCheckpoint an in-process node writes — the record
// `iterion fork --turn` resolves and rehydrates from.
func TestSandboxRelay_TurnCaptureCrossesIntoTheRunsTurnStore(t *testing.T) {
	if !relayCarries(func(h EventHooks) bool { return h.OnLLMTurnCapture != nil }) {
		t.Fatal("the runner's relay carries no turn capture: `iterion fork --turn` has nothing to anchor on for a sandboxed claw node")
	}
	const runID = "run-relay-turn"
	b, st := relayHostStore(t, runID)
	conv := clawTurnConversation()
	errs := runRelay(t, b, delegate.Task{NodeID: "campaign"}, func(relay EventHooks) {
		relay.OnLLMTurnCapture("campaign", LLMTurnCaptureInfo{
			Step:         2,
			Text:         "reading the tests first",
			ToolCalls:    []ToolCallEntry{{Name: "read_file", Input: json.RawMessage(`{"path":"main.go"}`)}},
			FinishReason: "tool_use",
			InputTokens:  9000,
			OutputTokens: 120,
			Iteration:    1,
			conversation: conv,
		})
	})
	if len(errs) != 0 {
		t.Fatalf("relay reported %v", errs)
	}

	ctx := context.Background()
	turn, err := store.AsTurnStore(st).LatestTurn(ctx, runID, "campaign")
	if err != nil {
		t.Fatalf("LatestTurn (the seam `iterion fork --turn` resolves): %v", err)
	}
	if turn.LoopIter != 1 || turn.TurnIndex != 1 {
		t.Errorf("turn anchored at loop_iter=%d turn_index=%d, want 1/1 (step 2 of iteration 1)", turn.LoopIter, turn.TurnIndex)
	}
	if turn.Backend != delegate.BackendClaw || turn.FinishReason != "tool_use" {
		t.Errorf("turn = %+v, want the claw backend and its finish reason", turn)
	}
	if len(turn.ToolCalls) != 1 || turn.ToolCalls[0].Name != "read_file" {
		t.Errorf("turn.ToolCalls = %+v, want the step's call", turn.ToolCalls)
	}
	if turn.Usage.InputTokens != 9000 || turn.Usage.OutputTokens != 120 {
		t.Errorf("turn.Usage = %+v, want the step's tokens", turn.Usage)
	}
	if turn.MessagesRef == "" {
		t.Fatal("turn carries no MessagesRef: Fork would rehydrate the child with no conversation")
	}
	msgs, err := store.AsTurnStore(st).LoadTurnMessages(ctx, runID, "campaign", turn.LoopIter, turn.TurnIndex)
	if err != nil {
		t.Fatalf("LoadTurnMessages: %v", err)
	}
	var got []api.Message
	if err := json.Unmarshal(msgs, &got); err != nil {
		t.Fatalf("the persisted snapshot is not a claw conversation: %v", err)
	}
	if len(got) != len(conv) || got[0].Content[0].Text != "ship the feature" ||
		got[1].Content[1].Name != "read_file" {
		t.Fatalf("rehydrated conversation = %+v, want the snapshot the container captured", got)
	}
}

// #811, decision (a) — the IPC refuses a line over
// delegate.MaxEnvelopeLineBytes by killing the channel, so an oversize
// payload is CUT with a marker the consumer can read, never dropped in
// silence and never written as a line the launcher cannot parse. The
// event still lands, and the honest size travels beside the cut.
func TestSandboxRelay_OversizeToolPayloadsAreMarkedNotDropped(t *testing.T) {
	if !relayCarries(func(h EventHooks) bool { return h.OnToolStarted != nil && h.OnToolCall != nil }) {
		t.Fatal("the runner's relay carries no tool hooks: nothing to size-check")
	}
	const runID = "run-relay-oversize"
	b, st := relayHostStore(t, runID)
	load := func() []*store.Event {
		evts, err := st.LoadEvents(context.Background(), runID)
		if err != nil {
			t.Fatalf("LoadEvents: %v", err)
		}
		return evts
	}
	hugeOutput := strings.Repeat("x", 3*1024*1024)
	hugeInput := json.RawMessage(`{"content":"` + strings.Repeat("y", 3*1024*1024) + `"}`)
	errs := runRelay(t, b, delegate.Task{NodeID: "campaign"}, func(relay EventHooks) {
		relay.OnToolStarted("campaign", LLMToolStartedInfo{
			ToolName: "write_file", ToolUseID: "tu_1", InputSize: len(hugeInput), Input: hugeInput,
		})
		relay.OnToolCall("campaign", LLMToolCallInfo{
			ToolName: "bash", ToolUseID: "tu_1", InputSize: len(hugeInput), Output: hugeOutput,
		})
	})
	if len(errs) != 0 {
		t.Fatalf("relay reported %v — an oversize payload must be cut, not fail the channel", errs)
	}

	evts := load()
	started := eventsOfType(evts, store.EventToolStarted)
	if len(started) != 1 {
		t.Fatalf("tool_started events = %d, want 1 — the oversize input dropped the whole event", len(started))
	}
	if started[0].Data["input_size"] != float64(len(hugeInput)) {
		t.Errorf("tool_started input_size = %v, want the ORIGINAL %d bytes", started[0].Data["input_size"], len(hugeInput))
	}
	// The input is a JSON document the studio parses: cut, it must stay
	// valid JSON and name what was left behind.
	inline := fmt.Sprint(started[0].Data["input"]) + fmt.Sprint(started[0].Data["input_preview"])
	if !strings.Contains(inline, "relay") {
		t.Errorf("tool_started input = %.200q, want a marker naming the sandbox relay as the cutter", inline)
	}

	called := eventsOfType(evts, store.EventToolCalled)
	if len(called) != 1 {
		t.Fatalf("tool_called events = %d, want 1 — the oversize output dropped the whole event", len(called))
	}
	// The cut output is what the host persisted: shorter than the
	// original, and ending on a marker that names the relay as the
	// cutter — a reader must never mistake it for the tool's real answer.
	body, total, _, err := st.ReadToolBlob(context.Background(), runID, "tu_1", "output", 0, 0)
	if err != nil {
		t.Fatalf("ReadToolBlob: %v", err)
	}
	if total >= int64(len(hugeOutput)) {
		t.Errorf("persisted output = %d bytes, want less than the %d the container held", total, len(hugeOutput))
	}
	if tail := string(body[max(0, len(body)-200):]); !strings.Contains(tail, "relay") {
		t.Errorf("persisted output ends %.200q, want a marker naming the sandbox relay as the cutter", tail)
	}
}

// #811, decision (b) — a conversation snapshot bigger than the relay can
// carry is OMITTED whole (a truncated one is not a conversation), and the
// turn still lands, naming the bytes that stayed in the container.
func TestSandboxRelay_OversizeConversationIsOmittedAndNamed(t *testing.T) {
	if !relayCarries(func(h EventHooks) bool { return h.OnLLMTurnCapture != nil }) {
		t.Fatal("the runner's relay carries no turn capture: nothing to size-check")
	}
	const runID = "run-relay-oversize-conv"
	b, st := relayHostStore(t, runID)
	huge := make([]api.Message, 0, 400)
	for i := 0; i < 400; i++ {
		huge = append(huge, api.Message{Role: "user", Content: []api.ContentBlock{
			{Type: "text", Text: strings.Repeat("z", 8*1024)},
		}})
	}
	errs := runRelay(t, b, delegate.Task{NodeID: "campaign"}, func(relay EventHooks) {
		relay.OnLLMTurnCapture("campaign", LLMTurnCaptureInfo{
			Step: 1, FinishReason: "tool_use", InputTokens: 800000, conversation: huge,
		})
	})
	if len(errs) != 0 {
		t.Fatalf("relay reported %v — an oversize conversation must degrade, not fail the channel", errs)
	}
	turn, err := store.AsTurnStore(st).LatestTurn(context.Background(), runID, "campaign")
	if err != nil {
		t.Fatalf("LatestTurn: %v — the turn itself must survive its conversation not fitting", err)
	}
	if turn.MessagesRef != "" {
		t.Errorf("turn.MessagesRef = %q, want empty: a truncated conversation must never be presented as one", turn.MessagesRef)
	}
	if turn.FinishReason != "tool_use" || turn.Usage.InputTokens != 800000 {
		t.Errorf("turn = %+v, want the metadata intact", turn)
	}
}

// #811 — the plan checklist the studio renders is built from the INPUT of
// the agent's `todo_write` calls, which the tool-started hook captures. On
// a sandboxed claw node that call happens in the container, so the plan
// store stays empty until the relay carries the input across.
func TestSandboxRelay_RelayedTodoWriteFeedsThePlanStore(t *testing.T) {
	if !relayCarries(func(h EventHooks) bool { return h.OnToolStarted != nil }) {
		t.Fatal("the runner's relay carries no tool_started: a sandboxed node's plan never reaches the studio")
	}
	const runID = "run-relay-plan"
	b, st := relayHostStore(t, runID)
	errs := runRelay(t, b, delegate.Task{NodeID: "campaign"}, func(relay EventHooks) {
		relay.OnToolStarted("campaign", LLMToolStartedInfo{
			ToolName:  "todo_write",
			ToolUseID: "tu_9",
			Input:     json.RawMessage(`{"todos":[{"content":"wire the relay","status":"in_progress"}]}`),
			Iteration: 2,
		})
	})
	if len(errs) != 0 {
		t.Fatalf("relay reported %v", errs)
	}
	snaps, err := st.ListPlanSnapshots(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListPlanSnapshots: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("plan snapshots = %d, want 1 — the container's todo_write left no plan", len(snaps))
	}
	if len(snaps[0].Todos) != 1 || snaps[0].Todos[0].Content != "wire the relay" || snaps[0].Iteration != 2 {
		t.Errorf("plan snapshot = %+v, want the container's todo at iteration 2", snaps[0])
	}
}

// #811, decision (a) again — the numbers a step carries are what the run
// is METERED on, so an event whose bulk pushes the line past the cap must
// still cross: the relay drops the bulk, marks the drop, and delivers the
// counts. Losing the whole event here would leave the node billed from
// the delegation total the launcher deliberately stops counting once it
// has seen steps.
func TestSandboxRelay_ABulkyStepStillDeliversItsTokenCounts(t *testing.T) {
	b, load := relayHost(t, "run-relay-bulky-step")
	// Six tool inputs, each just under the per-field budget: no single
	// field is clamped, their sum is past the IPC's per-line cap.
	fat := json.RawMessage(`{"content":"` + strings.Repeat("w", 900*1024) + `"}`)
	calls := make([]ToolCallEntry, 6)
	for i := range calls {
		calls[i] = ToolCallEntry{Name: "write_file", Input: fat}
	}
	errs := runRelay(t, b, delegate.Task{NodeID: "campaign"}, func(relay EventHooks) {
		relay.OnLLMStepFinish("campaign", LLMStepInfo{
			Number: 4, ToolCalls: calls, FinishReason: "tool_use",
			InputTokens: 31000, OutputTokens: 4200, Iteration: 1,
		})
	})
	if len(errs) != 0 {
		t.Fatalf("relay reported %v — the step's tokens must reach the host even when its payloads cannot", errs)
	}
	steps := eventsOfType(load(), store.EventLLMStepFinished)
	if len(steps) != 1 {
		t.Fatalf("llm_step_finished events = %d, want 1 — the node is metered from this event", len(steps))
	}
	if steps[0].Data["input_tokens"] != float64(31000) || steps[0].Data["output_tokens"] != float64(4200) ||
		steps[0].Data["tool_calls"] != float64(6) {
		t.Errorf("llm_step_finished data = %v, want the token counts and call count intact", steps[0].Data)
	}
}

// #811, decision (a), last rung — a payload made of many SHORT strings
// has no bulk to cut, so it cannot be brought under the line cap. It is
// refused rather than written (writing it would fail the launcher's
// reader and with it the run's whole IPC) and the refusal is REPORTED on
// the runner's stderr, which the launcher folds into the node's error.
// The one lossy case in the relay is never a silent one.
func TestSandboxRelay_AnUnshrinkablePayloadIsRefusedAndReported(t *testing.T) {
	b, load := relayHost(t, "run-relay-unshrinkable")
	calls := make([]ToolCallEntry, 60000)
	for i := range calls {
		calls[i] = ToolCallEntry{Name: strings.Repeat("t", 60), Input: json.RawMessage(`{"a":"bbbbbbbb"}`)}
	}
	errs := runRelay(t, b, delegate.Task{NodeID: "campaign"}, func(relay EventHooks) {
		relay.OnLLMStepFinish("campaign", LLMStepInfo{Number: 1, ToolCalls: calls, InputTokens: 10})
	})
	if len(errs) != 1 {
		t.Fatalf("reported = %v, want exactly one refusal — a dropped observation must leave a trace", errs)
	}
	if !strings.Contains(errs[0].Error(), "llm_step_finished") || !strings.Contains(errs[0].Error(), "line cap") {
		t.Errorf("reported = %q, want the event type and the reason", errs[0])
	}
	if evts := eventsOfType(load(), store.EventLLMStepFinished); len(evts) != 0 {
		t.Errorf("llm_step_finished events = %d, want 0 — the line was refused, not truncated on the wire", len(evts))
	}
}
