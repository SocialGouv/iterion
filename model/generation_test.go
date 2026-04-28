package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
)

// ---------------------------------------------------------------------------
// Mock APIClient
// ---------------------------------------------------------------------------

// mockAPIClient is a deterministic mock that returns scripted StreamEvent
// sequences. Each call to StreamResponse pops the next script.
type mockAPIClient struct {
	mu      sync.Mutex
	scripts [][]api.StreamEvent // one per StreamResponse call
	calls   []api.CreateMessageRequest
}

func newMockClient(scripts ...[]api.StreamEvent) *mockAPIClient {
	return &mockAPIClient{scripts: scripts}
}

func (m *mockAPIClient) StreamResponse(_ context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls = append(m.calls, req)

	if len(m.scripts) == 0 {
		return nil, fmt.Errorf("mockAPIClient: no more scripts")
	}
	script := m.scripts[0]
	m.scripts = m.scripts[1:]

	ch := make(chan api.StreamEvent, len(script))
	for _, ev := range script {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func (m *mockAPIClient) getCalls() []api.CreateMessageRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]api.CreateMessageRequest(nil), m.calls...)
}

// ---------------------------------------------------------------------------
// Helper: build common event sequences
// ---------------------------------------------------------------------------

func textEvents(text string, inputTokens, outputTokens int) []api.StreamEvent {
	return []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: inputTokens},
		{Type: api.EventContentBlockStart, ContentBlock: api.ContentBlockInfo{Type: "text", Index: 0}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "text_delta", Text: text}},
		{Type: api.EventContentBlockStop, Index: 0},
		{Type: api.EventMessageDelta, StopReason: "end_turn", Usage: api.UsageDelta{OutputTokens: outputTokens}},
		{Type: api.EventMessageStop},
	}
}

func toolUseEvents(id, name, inputJSON string, inputTokens, outputTokens int) []api.StreamEvent {
	return []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: inputTokens},
		{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "tool_use", Index: 0, ID: id, Name: name}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "input_json_delta", PartialJSON: inputJSON}},
		{Type: api.EventContentBlockStop, Index: 0},
		{Type: api.EventMessageDelta, StopReason: "tool_use", Usage: api.UsageDelta{OutputTokens: outputTokens}},
		{Type: api.EventMessageStop},
	}
}

// ---------------------------------------------------------------------------
// Tests: aggregateStream
// ---------------------------------------------------------------------------

func TestAggregateStream(t *testing.T) {
	ch := make(chan api.StreamEvent, 10)
	events := textEvents("Hello, world!", 100, 20)
	for _, ev := range events {
		ch <- ev
	}
	close(ch)

	agg := aggregateStream(context.Background(), ch)
	if agg.err != nil {
		t.Fatalf("unexpected error: %v", agg.err)
	}
	if agg.text != "Hello, world!" {
		t.Errorf("text = %q, want %q", agg.text, "Hello, world!")
	}
	if agg.stopReason != "end_turn" {
		t.Errorf("stopReason = %q, want %q", agg.stopReason, "end_turn")
	}
	if agg.usage.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", agg.usage.InputTokens)
	}
	if agg.usage.OutputTokens != 20 {
		t.Errorf("OutputTokens = %d, want 20", agg.usage.OutputTokens)
	}
	if len(agg.toolUses) != 0 {
		t.Errorf("toolUses = %d, want 0", len(agg.toolUses))
	}
}

func TestAggregateStream_ToolUse(t *testing.T) {
	ch := make(chan api.StreamEvent, 10)
	events := []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: 50},
		{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "tool_use", Index: 0, ID: "tu_1", Name: "get_weather"}},
		// Partial JSON concatenation
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "input_json_delta", PartialJSON: `{"cit`}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "input_json_delta", PartialJSON: `y": "Paris"}`}},
		{Type: api.EventContentBlockStop, Index: 0},
		{Type: api.EventMessageDelta, StopReason: "tool_use", Usage: api.UsageDelta{OutputTokens: 30}},
		{Type: api.EventMessageStop},
	}
	for _, ev := range events {
		ch <- ev
	}
	close(ch)

	agg := aggregateStream(context.Background(), ch)
	if agg.err != nil {
		t.Fatalf("unexpected error: %v", agg.err)
	}
	if len(agg.toolUses) != 1 {
		t.Fatalf("toolUses = %d, want 1", len(agg.toolUses))
	}
	tu := agg.toolUses[0]
	if tu.ID != "tu_1" {
		t.Errorf("ID = %q, want %q", tu.ID, "tu_1")
	}
	if tu.Name != "get_weather" {
		t.Errorf("Name = %q, want %q", tu.Name, "get_weather")
	}
	if tu.PartialJSON != `{"city": "Paris"}` {
		t.Errorf("PartialJSON = %q, want %q", tu.PartialJSON, `{"city": "Paris"}`)
	}
	if agg.stopReason != "tool_use" {
		t.Errorf("stopReason = %q, want %q", agg.stopReason, "tool_use")
	}
}

func TestAggregateStream_Error(t *testing.T) {
	ch := make(chan api.StreamEvent, 5)
	ch <- api.StreamEvent{Type: api.EventMessageStart, InputTokens: 10}
	ch <- api.StreamEvent{Type: api.EventError, ErrorMessage: "rate limit exceeded"}
	close(ch)

	agg := aggregateStream(context.Background(), ch)
	if agg.err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := agg.err.Error(); got != "stream error: rate limit exceeded" {
		t.Errorf("error = %q, want %q", got, "stream error: rate limit exceeded")
	}
}

func TestAggregateStream_MultipleBlocks(t *testing.T) {
	ch := make(chan api.StreamEvent, 20)
	events := []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: 100},
		// Block 0: text
		{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "text", Index: 0}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "text_delta", Text: "Let me check "}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "text_delta", Text: "the weather."}},
		{Type: api.EventContentBlockStop, Index: 0},
		// Block 1: tool_use
		{Type: api.EventContentBlockStart, Index: 1, ContentBlock: api.ContentBlockInfo{Type: "tool_use", Index: 1, ID: "tu_1", Name: "get_weather"}},
		{Type: api.EventContentBlockDelta, Index: 1, Delta: api.Delta{Type: "input_json_delta", PartialJSON: `{"city": "NYC"}`}},
		{Type: api.EventContentBlockStop, Index: 1},
		// Block 2: another tool_use
		{Type: api.EventContentBlockStart, Index: 2, ContentBlock: api.ContentBlockInfo{Type: "tool_use", Index: 2, ID: "tu_2", Name: "get_time"}},
		{Type: api.EventContentBlockDelta, Index: 2, Delta: api.Delta{Type: "input_json_delta", PartialJSON: `{"tz": "EST"}`}},
		{Type: api.EventContentBlockStop, Index: 2},
		{Type: api.EventMessageDelta, StopReason: "tool_use", Usage: api.UsageDelta{OutputTokens: 50}},
		{Type: api.EventMessageStop},
	}
	for _, ev := range events {
		ch <- ev
	}
	close(ch)

	agg := aggregateStream(context.Background(), ch)
	if agg.err != nil {
		t.Fatalf("unexpected error: %v", agg.err)
	}
	if agg.text != "Let me check the weather." {
		t.Errorf("text = %q, want %q", agg.text, "Let me check the weather.")
	}
	if len(agg.toolUses) != 2 {
		t.Fatalf("toolUses = %d, want 2", len(agg.toolUses))
	}
	if agg.toolUses[0].Name != "get_weather" {
		t.Errorf("toolUses[0].Name = %q, want %q", agg.toolUses[0].Name, "get_weather")
	}
	if agg.toolUses[1].Name != "get_time" {
		t.Errorf("toolUses[1].Name = %q, want %q", agg.toolUses[1].Name, "get_time")
	}
}

func TestAggregateStream_IncompleteToolUse(t *testing.T) {
	ch := make(chan api.StreamEvent, 10)
	events := []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: 10},
		{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "tool_use", Index: 0, ID: "tu_1", Name: "broken"}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "input_json_delta", PartialJSON: `{"partial`}},
		// Missing content_block_stop!
		{Type: api.EventMessageDelta, StopReason: "end_turn", Usage: api.UsageDelta{OutputTokens: 5}},
		{Type: api.EventMessageStop},
	}
	for _, ev := range events {
		ch <- ev
	}
	close(ch)

	agg := aggregateStream(context.Background(), ch)
	if agg.err == nil {
		t.Fatal("expected error for incomplete tool_use, got nil")
	}
	if got := agg.err.Error(); got != "incomplete tool_use block: broken (content_block_stop not received)" {
		t.Errorf("error = %q", got)
	}
}

func TestAggregateStream_CacheTokens(t *testing.T) {
	ch := make(chan api.StreamEvent, 5)
	ch <- api.StreamEvent{
		Type:                     api.EventMessageStart,
		InputTokens:              200,
		CacheReadInputTokens:     150,
		CacheCreationInputTokens: 50,
	}
	ch <- api.StreamEvent{Type: api.EventContentBlockStart, ContentBlock: api.ContentBlockInfo{Type: "text", Index: 0}}
	ch <- api.StreamEvent{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "text_delta", Text: "ok"}}
	ch <- api.StreamEvent{Type: api.EventContentBlockStop, Index: 0}
	ch <- api.StreamEvent{Type: api.EventMessageDelta, StopReason: "end_turn", Usage: api.UsageDelta{OutputTokens: 10}}
	close(ch)

	agg := aggregateStream(context.Background(), ch)
	if agg.err != nil {
		t.Fatalf("unexpected error: %v", agg.err)
	}
	if agg.usage.CacheReadTokens != 150 {
		t.Errorf("CacheReadTokens = %d, want 150", agg.usage.CacheReadTokens)
	}
	if agg.usage.CacheWriteTokens != 50 {
		t.Errorf("CacheWriteTokens = %d, want 50", agg.usage.CacheWriteTokens)
	}
}

// ---------------------------------------------------------------------------
// Tests: GenerateTextDirect
// ---------------------------------------------------------------------------

func TestGenerateTextDirect_NoTools(t *testing.T) {
	client := newMockClient(textEvents("Hello!", 100, 20))

	result, err := GenerateTextDirect(context.Background(), client, GenerationOptions{
		Model:  "claude-sonnet-4-6",
		System: "You are helpful.",
		Messages: []api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "Hi"}}},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "Hello!" {
		t.Errorf("Text = %q, want %q", result.Text, "Hello!")
	}
	if result.FinishReason != FinishStop {
		t.Errorf("FinishReason = %q, want %q", result.FinishReason, FinishStop)
	}
	if len(result.Steps) != 1 {
		t.Fatalf("Steps = %d, want 1", len(result.Steps))
	}
	if result.TotalUsage.InputTokens != 100 {
		t.Errorf("TotalUsage.InputTokens = %d, want 100", result.TotalUsage.InputTokens)
	}
	if result.TotalUsage.OutputTokens != 20 {
		t.Errorf("TotalUsage.OutputTokens = %d, want 20", result.TotalUsage.OutputTokens)
	}

	// Verify the request was built correctly.
	calls := client.getCalls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].Model != "claude-sonnet-4-6" {
		t.Errorf("Model = %q, want %q", calls[0].Model, "claude-sonnet-4-6")
	}
	if calls[0].System != "You are helpful." {
		t.Errorf("System = %q, want %q", calls[0].System, "You are helpful.")
	}
	if calls[0].MaxTokens != defaultMaxTokens {
		t.Errorf("MaxTokens = %d, want %d", calls[0].MaxTokens, defaultMaxTokens)
	}
}

func TestGenerateTextDirect_ToolLoop(t *testing.T) {
	// Step 1: model calls tool "add" with {a:2,b:3}
	// Step 2: model returns "The sum is 5"
	client := newMockClient(
		toolUseEvents("tu_1", "add", `{"a":2,"b":3}`, 100, 30),
		textEvents("The sum is 5", 150, 25),
	)

	addTool := GenerationTool{
		Name:        "add",
		Description: "Adds two numbers",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"number"},"b":{"type":"number"}}}`),
		Execute: func(_ context.Context, input json.RawMessage) (string, error) {
			var args struct {
				A float64 `json:"a"`
				B float64 `json:"b"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", err
			}
			return fmt.Sprintf("%.0f", args.A+args.B), nil
		},
	}

	result, err := GenerateTextDirect(context.Background(), client, GenerationOptions{
		Model: "claude-sonnet-4-6",
		Tools: []GenerationTool{addTool},
		Messages: []api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "What is 2+3?"}}},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "The sum is 5" {
		t.Errorf("Text = %q, want %q", result.Text, "The sum is 5")
	}
	if len(result.Steps) != 2 {
		t.Fatalf("Steps = %d, want 2", len(result.Steps))
	}

	// Step 1 should have tool calls.
	if len(result.Steps[0].ToolCalls) != 1 {
		t.Fatalf("Steps[0].ToolCalls = %d, want 1", len(result.Steps[0].ToolCalls))
	}
	if result.Steps[0].ToolCalls[0].Name != "add" {
		t.Errorf("ToolCalls[0].Name = %q, want %q", result.Steps[0].ToolCalls[0].Name, "add")
	}

	// Step 2 should have no tool calls.
	if len(result.Steps[1].ToolCalls) != 0 {
		t.Errorf("Steps[1].ToolCalls = %d, want 0", len(result.Steps[1].ToolCalls))
	}

	// Usage should be aggregated.
	if result.TotalUsage.InputTokens != 250 {
		t.Errorf("TotalUsage.InputTokens = %d, want 250", result.TotalUsage.InputTokens)
	}

	// Verify second call includes tool results.
	calls := client.getCalls()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	// Second call should have 3 messages: original user + assistant tool_use + user tool_result
	if len(calls[1].Messages) != 3 {
		t.Errorf("second call messages = %d, want 3", len(calls[1].Messages))
	}
}

func TestGenerateTextDirect_MaxSteps(t *testing.T) {
	// Model always calls a tool — should be limited by MaxSteps.
	scripts := make([][]api.StreamEvent, 5)
	for i := range scripts {
		scripts[i] = toolUseEvents(fmt.Sprintf("tu_%d", i), "loop_tool", `{}`, 10, 5)
	}
	client := newMockClient(scripts...)

	loopTool := GenerationTool{
		Name:        "loop_tool",
		Description: "Always called",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "ok", nil
		},
	}

	result, err := GenerateTextDirect(context.Background(), client, GenerationOptions{
		Model:    "claude-sonnet-4-6",
		Tools:    []GenerationTool{loopTool},
		MaxSteps: 3,
		Messages: []api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "loop"}}},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should have exactly 3 steps (max).
	if len(result.Steps) != 3 {
		t.Errorf("Steps = %d, want 3", len(result.Steps))
	}
	if result.FinishReason != FinishToolCalls {
		t.Errorf("FinishReason = %q, want %q", result.FinishReason, FinishToolCalls)
	}
}

// ---------------------------------------------------------------------------
// Tests: GenerateObjectDirect
// ---------------------------------------------------------------------------

func TestGenerateObjectDirect(t *testing.T) {
	type Weather struct {
		City string `json:"city"`
		Temp int    `json:"temp"`
	}

	// Model returns a tool_use block with the structured output.
	events := []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: 80},
		{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "tool_use", Index: 0, ID: "tu_1", Name: "structured_output"}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "input_json_delta", PartialJSON: `{"city":"Paris","temp":22}`}},
		{Type: api.EventContentBlockStop, Index: 0},
		{Type: api.EventMessageDelta, StopReason: "tool_use", Usage: api.UsageDelta{OutputTokens: 15}},
		{Type: api.EventMessageStop},
	}
	client := newMockClient(events)

	schema := json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"},"temp":{"type":"integer"}},"required":["city","temp"]}`)

	result, err := GenerateObjectDirect[Weather](context.Background(), client, GenerationOptions{
		Model:          "claude-sonnet-4-6",
		ExplicitSchema: schema,
		Messages: []api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "Weather in Paris?"}}},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Object.City != "Paris" {
		t.Errorf("City = %q, want %q", result.Object.City, "Paris")
	}
	if result.Object.Temp != 22 {
		t.Errorf("Temp = %d, want 22", result.Object.Temp)
	}

	// Verify ToolChoice was set in the request.
	calls := client.getCalls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].ToolChoice == nil {
		t.Fatal("ToolChoice is nil")
	}
	if calls[0].ToolChoice.Type != "tool" {
		t.Errorf("ToolChoice.Type = %q, want %q", calls[0].ToolChoice.Type, "tool")
	}
	if calls[0].ToolChoice.Name != "structured_output" {
		t.Errorf("ToolChoice.Name = %q, want %q", calls[0].ToolChoice.Name, "structured_output")
	}

	// Verify synthetic tool was injected.
	if len(calls[0].Tools) != 1 {
		t.Fatalf("Tools = %d, want 1", len(calls[0].Tools))
	}
	if calls[0].Tools[0].Name != "structured_output" {
		t.Errorf("Tool name = %q, want %q", calls[0].Tools[0].Name, "structured_output")
	}
}

func TestGenerateObjectDirect_NoSchema(t *testing.T) {
	type Dummy struct{}
	client := newMockClient()

	_, err := GenerateObjectDirect[Dummy](context.Background(), client, GenerationOptions{
		Model: "claude-sonnet-4-6",
	})
	if err == nil {
		t.Fatal("expected error for missing schema")
	}
	if got := err.Error(); got != "GenerateObjectDirect requires ExplicitSchema to be set" {
		t.Errorf("error = %q", got)
	}
}

func TestGenerateObjectDirect_CustomSchemaName(t *testing.T) {
	type Result struct {
		Value int `json:"value"`
	}

	events := []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: 50},
		{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "tool_use", Index: 0, ID: "tu_1", Name: "my_schema"}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "input_json_delta", PartialJSON: `{"value":42}`}},
		{Type: api.EventContentBlockStop, Index: 0},
		{Type: api.EventMessageDelta, StopReason: "tool_use", Usage: api.UsageDelta{OutputTokens: 10}},
		{Type: api.EventMessageStop},
	}
	client := newMockClient(events)

	result, err := GenerateObjectDirect[Result](context.Background(), client, GenerationOptions{
		Model:          "claude-sonnet-4-6",
		ExplicitSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"integer"}}}`),
		SchemaName:     "my_schema",
		Messages: []api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "gimme 42"}}},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Object.Value != 42 {
		t.Errorf("Value = %d, want 42", result.Object.Value)
	}

	calls := client.getCalls()
	if calls[0].ToolChoice.Name != "my_schema" {
		t.Errorf("ToolChoice.Name = %q, want %q", calls[0].ToolChoice.Name, "my_schema")
	}
}

// ---------------------------------------------------------------------------
// Tests: Hook ordering
// ---------------------------------------------------------------------------

func TestHookOrdering(t *testing.T) {
	client := newMockClient(textEvents("done", 50, 10))

	var order []string

	result, err := GenerateTextDirect(context.Background(), client, GenerationOptions{
		Model: "claude-sonnet-4-6",
		Messages: []api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}},
		},
		OnRequest: func(info RequestInfo) {
			order = append(order, "request")
			if info.Model != "claude-sonnet-4-6" {
				t.Errorf("RequestInfo.Model = %q", info.Model)
			}
			if info.MessageCount != 1 {
				t.Errorf("RequestInfo.MessageCount = %d, want 1", info.MessageCount)
			}
		},
		OnResponse: func(info ResponseInfo) {
			order = append(order, "response")
			if info.Usage.InputTokens != 50 {
				t.Errorf("ResponseInfo.Usage.InputTokens = %d, want 50", info.Usage.InputTokens)
			}
			if info.FinishReason != FinishStop {
				t.Errorf("ResponseInfo.FinishReason = %q, want %q", info.FinishReason, FinishStop)
			}
		},
		OnStepFinish: func(step StepResult) {
			order = append(order, "step")
			if step.Number != 1 {
				t.Errorf("StepResult.Number = %d, want 1", step.Number)
			}
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = result

	expected := []string{"request", "response", "step"}
	if len(order) != len(expected) {
		t.Fatalf("hook calls = %v, want %v", order, expected)
	}
	for i, e := range expected {
		if order[i] != e {
			t.Errorf("order[%d] = %q, want %q", i, order[i], e)
		}
	}
}

func TestHookOrdering_WithToolCall(t *testing.T) {
	client := newMockClient(
		toolUseEvents("tu_1", "echo", `{"msg":"hi"}`, 50, 10),
		textEvents("echoed", 80, 15),
	)

	var order []string

	echoTool := GenerationTool{
		Name:        "echo",
		Description: "Echoes input",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"msg":{"type":"string"}}}`),
		Execute: func(_ context.Context, input json.RawMessage) (string, error) {
			return string(input), nil
		},
	}

	_, err := GenerateTextDirect(context.Background(), client, GenerationOptions{
		Model:    "claude-sonnet-4-6",
		Tools:    []GenerationTool{echoTool},
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "echo"}}}},
		OnRequest: func(_ RequestInfo) {
			order = append(order, "request")
		},
		OnResponse: func(_ ResponseInfo) {
			order = append(order, "response")
		},
		OnStepFinish: func(_ StepResult) {
			order = append(order, "step")
		},
		OnToolCall: func(info ToolCallInfo) {
			order = append(order, "toolcall:"+info.ToolName)
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Expected: request → response → step → toolcall → request → response → step
	expected := []string{"request", "response", "step", "toolcall:echo", "request", "response", "step"}
	if len(order) != len(expected) {
		t.Fatalf("hook calls = %v, want %v", order, expected)
	}
	for i, e := range expected {
		if order[i] != e {
			t.Errorf("order[%d] = %q, want %q", i, order[i], e)
		}
	}
}

// ---------------------------------------------------------------------------
// Tests: Cancellation
// ---------------------------------------------------------------------------

func TestGenerateTextDirect_Cancellation(t *testing.T) {
	// Create a channel that we control.
	ch := make(chan api.StreamEvent, 2)
	ch <- api.StreamEvent{Type: api.EventMessageStart, InputTokens: 10}
	// Don't close — simulate slow stream.

	ctx, cancel := context.WithCancel(context.Background())

	// Create a client that returns our controlled channel.
	client := &cancellationClient{ch: ch}

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := GenerateTextDirect(ctx, client, GenerationOptions{
		Model:    "claude-sonnet-4-6",
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

type cancellationClient struct {
	ch chan api.StreamEvent
}

func (c *cancellationClient) StreamResponse(_ context.Context, _ api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	return c.ch, nil
}

// ---------------------------------------------------------------------------
// Tests: buildRequest
// ---------------------------------------------------------------------------

func TestBuildRequest(t *testing.T) {
	temp := 0.7
	opts := GenerationOptions{
		Model:       "claude-sonnet-4-6",
		System:      "Be concise.",
		MaxTokens:   4096,
		Temperature: &temp,
		Tools: []GenerationTool{
			{
				Name:        "search",
				Description: "Search the web",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string","description":"query"}},"required":["q"]}`),
			},
		},
	}
	messages := []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hello"}}},
	}

	req, err := buildRequest(opts, messages, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if req.Model != "claude-sonnet-4-6" {
		t.Errorf("Model = %q", req.Model)
	}
	if req.System != "Be concise." {
		t.Errorf("System = %q", req.System)
	}
	if req.MaxTokens != 4096 {
		t.Errorf("MaxTokens = %d, want 4096", req.MaxTokens)
	}
	if req.Temperature == nil || *req.Temperature != 0.7 {
		t.Errorf("Temperature = %v", req.Temperature)
	}
	if len(req.Tools) != 1 {
		t.Fatalf("Tools = %d, want 1", len(req.Tools))
	}
	if req.Tools[0].Name != "search" {
		t.Errorf("Tool name = %q", req.Tools[0].Name)
	}
	if req.ToolChoice != nil {
		t.Errorf("ToolChoice should be nil")
	}
}

func TestBuildRequest_DefaultMaxTokens(t *testing.T) {
	req, err := buildRequest(GenerationOptions{Model: "test"}, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.MaxTokens != defaultMaxTokens {
		t.Errorf("MaxTokens = %d, want %d", req.MaxTokens, defaultMaxTokens)
	}
}

func TestBuildRequest_WithToolChoice(t *testing.T) {
	tc := &api.ToolChoice{Type: "tool", Name: "my_tool"}
	req, err := buildRequest(GenerationOptions{Model: "test"}, nil, nil, tc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.ToolChoice == nil {
		t.Fatal("ToolChoice is nil")
	}
	if req.ToolChoice.Name != "my_tool" {
		t.Errorf("ToolChoice.Name = %q", req.ToolChoice.Name)
	}
}

// ---------------------------------------------------------------------------
// Tests: mapStopReason
// ---------------------------------------------------------------------------

func TestMapStopReason(t *testing.T) {
	tests := []struct {
		reason string
		want   FinishReason
	}{
		{"end_turn", FinishStop},
		{"stop", FinishStop},
		{"tool_use", FinishToolCalls},
		{"max_tokens", FinishLength},
		{"content_filter", FinishContentFilter},
		{"unknown", FinishOther},
		{"", FinishOther},
	}
	for _, tt := range tests {
		if got := mapStopReason(tt.reason); got != tt.want {
			t.Errorf("mapStopReason(%q) = %q, want %q", tt.reason, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Tests: error from StreamResponse
// ---------------------------------------------------------------------------

func TestGenerateTextDirect_StreamResponseError(t *testing.T) {
	// Client returns an error immediately.
	client := &errorClient{err: fmt.Errorf("connection refused")}

	_, err := GenerateTextDirect(context.Background(), client, GenerationOptions{
		Model:    "claude-sonnet-4-6",
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); got != "connection refused" {
		t.Errorf("error = %q, want %q", got, "connection refused")
	}
}

type errorClient struct {
	err error
}

func (c *errorClient) StreamResponse(_ context.Context, _ api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	return nil, c.err
}

// ---------------------------------------------------------------------------
// Tests: OnResponse called on StreamResponse error
// ---------------------------------------------------------------------------

func TestGenerateTextDirect_OnResponseCalledOnError(t *testing.T) {
	client := &errorClient{err: fmt.Errorf("timeout")}
	var called bool

	_, _ = GenerateTextDirect(context.Background(), client, GenerationOptions{
		Model:    "claude-sonnet-4-6",
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
		OnResponse: func(info ResponseInfo) {
			called = true
			if info.Error == nil {
				t.Error("expected non-nil error in ResponseInfo")
			}
		},
	})
	if !called {
		t.Error("OnResponse was not called on StreamResponse error")
	}
}

// ---------------------------------------------------------------------------
// Tests: unknown tool in tool loop
// ---------------------------------------------------------------------------

func TestGenerateTextDirect_UnknownTool(t *testing.T) {
	client := newMockClient(
		toolUseEvents("tu_1", "nonexistent", `{}`, 50, 10),
		textEvents("ok", 60, 5),
	)

	var toolCallErrors []string

	_, err := GenerateTextDirect(context.Background(), client, GenerationOptions{
		Model: "claude-sonnet-4-6",
		Messages: []api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "call unknown"}}},
		},
		OnToolCall: func(info ToolCallInfo) {
			if info.Error != nil {
				toolCallErrors = append(toolCallErrors, info.Error.Error())
			}
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(toolCallErrors) != 1 {
		t.Fatalf("toolCallErrors = %d, want 1", len(toolCallErrors))
	}
	if toolCallErrors[0] != "unknown tool: nonexistent" {
		t.Errorf("error = %q", toolCallErrors[0])
	}
}
