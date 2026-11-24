package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/tool"
)

// The CM-7 wiring at the MCP seam: the registration closure's observation
// (observeRichResult) hands a result's structured content to the sink armed
// on the call's context, the LAST result wins — the Read auto-retry re-calls
// and its answer is the program's — and the observation leaves the
// model-facing render untouched.

// The full registration closure runs only inside live discovery, so the unit
// witnesses drive observeRichResult — the helper both the normal and the
// retry path call after a successful CallTool — over the same sink the
// script tool server arms.
func TestObserveRichResultRecordsTheLastObservation(t *testing.T) {
	t.Parallel()

	sink := tool.NewRichResultSink()
	ctx := tool.WithRichResultSink(context.Background(), sink)

	// Compact JSON: the payload travels to the script as the marshal of the
	// server's value, not as a decorated document.
	first := map[string]any{"matches": 7, "files": []any{"a.md", "b.md"}}
	observeRichResult(ctx, &ToolCallResult{StructuredContent: first})
	raw, ok := sink.Result()
	if !ok {
		t.Fatal("no observation reached the sink")
	}
	if want := `{"files":["a.md","b.md"],"matches":7}`; string(raw) != want {
		t.Errorf("the sink holds %s, want the compact marshal %s", raw, want)
	}

	// The retry path observes again and the retry's result REPLACES the
	// first: a script reading a shrunken Read's retry must not see the
	// oversized answer that came before it.
	observeRichResult(ctx, &ToolCallResult{StructuredContent: map[string]any{"matches": 2}})
	raw, ok = sink.Result()
	if !ok || string(raw) != `{"matches":2}` {
		t.Errorf("after the retry the sink holds %s (ok=%t), want the retry's payload", raw, ok)
	}
}

// A call with no structured content observes nothing, and a result the
// server's own encoding cannot marshal keeps the text path: the skip is the
// documented behaviour, and the sink holding nothing is how the script sees
// it.
func TestObserveRichResultSkipsWithoutStructuredContent(t *testing.T) {
	t.Parallel()

	sink := tool.NewRichResultSink()
	ctx := tool.WithRichResultSink(context.Background(), sink)

	observeRichResult(ctx, nil)
	observeRichResult(ctx, &ToolCallResult{Content: []ToolContent{{Type: "text", Text: "plain"}}})
	observeRichResult(ctx, &ToolCallResult{StructuredContent: make(chan int)})
	if _, ok := sink.Result(); ok {
		t.Error("the sink observed a call that sent no marshalable structured content")
	}
}

// No sink armed — the direct path — observes nothing, and the render of a
// result with both text and structured content keeps returning the TEXT
// before and after an observation: flattening stays the model-facing
// contract, byte for byte.
func TestObserveRichResultLeavesTheTextRenderUntouched(t *testing.T) {
	t.Parallel()

	result := &ToolCallResult{
		Content:           []ToolContent{{Type: "text", Text: "7 matches"}},
		StructuredContent: map[string]any{"matches": 7},
	}
	before, beforeErr := formatToolResult(result)

	// Without a sink (direct path): nothing to observe, render unchanged.
	observeRichResult(context.Background(), result)
	// With a sink (nested path): the observation lands, the render does not move.
	sink := tool.NewRichResultSink()
	observeRichResult(tool.WithRichResultSink(context.Background(), sink), result)
	after, afterErr := formatToolResult(result)

	if before != after || (beforeErr != nil) != (afterErr != nil) || (beforeErr != nil && beforeErr.Error() != afterErr.Error()) {
		t.Errorf("the render moved across the observation: (%q, %v) then (%q, %v)", before, beforeErr, after, afterErr)
	}
	if before != "7 matches" {
		t.Errorf("a result with text renders %q, want the text — not the flattened structured content", before)
	}
	raw, ok := sink.Result()
	if !ok || string(raw) != `{"matches":7}` {
		t.Errorf("the nested sink holds %s (ok=%t), want the observed payload", raw, ok)
	}
	if !json.Valid(raw) {
		t.Errorf("the observed payload %s is not valid JSON", raw)
	}
}

// ---------------------------------------------------------------------------
// The registration closure itself
// ---------------------------------------------------------------------------

// The helper-level witnesses above drive observeRichResult directly. The
// witnesses below drive the REAL registration closure — the one ensureServer
// builds — over a fake protocol client, so a call site that stops feeding
// the sink reddens here and not only at the helper.
type richFakeClient struct {
	calls   int
	results []*ToolCallResult
}

func (c *richFakeClient) Ping(context.Context) error { return nil }
func (c *richFakeClient) ListTools(context.Context) ([]ToolInfo, error) {
	return []ToolInfo{{Name: c.toolName(), Description: "probe tool", InputSchema: json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer"},"path":{"type":"string"}},"required":["path"]}`)}}, nil
}
func (c *richFakeClient) CallTool(context.Context, string, map[string]any) (*ToolCallResult, error) {
	c.calls++
	return c.results[min(c.calls, len(c.results))-1], nil
}
func (c *richFakeClient) ListResources(context.Context) ([]ResourceInfo, error) { return nil, nil }
func (c *richFakeClient) ReadResource(context.Context, string) (ResourceContent, error) {
	return ResourceContent{}, nil
}
func (c *richFakeClient) Close() error { return nil }

func (c *richFakeClient) toolName() string {
	if len(c.results) > 1 {
		return "Read" // two scripted answers: the auto-retry witness
	}
	return "Fetch"
}

// registeredProbeTool registers one server served by fake, pre-seeds the
// client so no process spawns, and returns the registry Execute closure the
// discovery built — the exact closure a model's call (direct or nested)
// lands in.
func registeredProbeTool(t *testing.T, fake *richFakeClient) func(context.Context, json.RawMessage) (string, error) {
	t.Helper()
	m := NewManager(map[string]*ServerConfig{
		"probe": {Name: "probe", Transport: TransportStdio, Command: "true"},
	}, WithStartPolicy(StartAllServers))
	state, err := m.state("probe")
	if err != nil {
		t.Fatal(err)
	}
	state.client = fake
	registry := tool.NewRegistry()
	if _, err := m.ensureServer(context.Background(), registry, "probe"); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	def, err := registry.Resolve("mcp.probe." + fake.toolName())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return def.Execute
}

// The closure's normal path: the text render is what the model reads, and
// the sink armed on the call's context receives the compact structured
// content.
func TestTheRegistrationClosureFeedsTheSink(t *testing.T) {
	t.Parallel()

	fake := &richFakeClient{results: []*ToolCallResult{{
		Content:           []ToolContent{{Type: "text", Text: "rendered"}},
		StructuredContent: map[string]any{"matches": 7},
	}}}
	execute := registeredProbeTool(t, fake)

	sink := tool.NewRichResultSink()
	text, err := execute(tool.WithRichResultSink(context.Background(), sink), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if text != "rendered" {
		t.Errorf("the closure rendered %q, want the text content unchanged", text)
	}
	if raw, ok := sink.Result(); !ok || string(raw) != `{"matches":7}` {
		t.Errorf("the closure fed the sink %s (ok=%t), want the compact structured content", raw, ok)
	}
	if fake.calls != 1 {
		t.Errorf("the fake served %d calls, want one — no retry outside its gate", fake.calls)
	}
}

// The auto-retry path: the first answer is an oversized Read whose shrink
// retry succeeds, and the sink must hold the RETRY's structured content —
// the last result is the program's answer, not the oversized one that
// preceded it.
func TestTheRegistrationClosureRetriesAndTheSinkHoldsTheRetry(t *testing.T) {
	t.Parallel()

	fake := &richFakeClient{results: []*ToolCallResult{
		{
			Content:           []ToolContent{{Type: "text", Text: "content exceeds maximum allowed tokens"}},
			IsError:           true,
			StructuredContent: map[string]any{"oversized": true},
		},
		{
			Content:           []ToolContent{{Type: "text", Text: "short content"}},
			StructuredContent: map[string]any{"matches": 3},
		},
	}}
	execute := registeredProbeTool(t, fake)

	sink := tool.NewRichResultSink()
	text, err := execute(tool.WithRichResultSink(context.Background(), sink), json.RawMessage(`{"path":"doc.md"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if text != "short content" {
		t.Errorf("the closure rendered %q, want the retry's text", text)
	}
	if fake.calls != 2 {
		t.Fatalf("the fake served %d calls, want the oversized first answer and the shrunken retry", fake.calls)
	}
	if raw, ok := sink.Result(); !ok || string(raw) != `{"matches":3}` {
		t.Errorf("the sink holds %s (ok=%t), want the retry's payload — the last result is the program's answer", raw, ok)
	}
}

// The retry without structured content: the failed first answer carried a
// structured payload and the shrunken retry succeeded with text only — the
// normal Read shape. The sink must end up EMPTY: a failed call's payload
// beside the retry's success is a stale pair the program would read as its
// own answer.
func TestARetryWithoutStructuredContentLeavesNoStalePayload(t *testing.T) {
	t.Parallel()

	fake := &richFakeClient{results: []*ToolCallResult{
		{
			Content:           []ToolContent{{Type: "text", Text: "content exceeds maximum allowed tokens"}},
			IsError:           true,
			StructuredContent: map[string]any{"oversized": true},
		},
		{
			Content: []ToolContent{{Type: "text", Text: "short content"}},
		},
	}}
	execute := registeredProbeTool(t, fake)

	sink := tool.NewRichResultSink()
	text, err := execute(tool.WithRichResultSink(context.Background(), sink), json.RawMessage(`{"path":"doc.md"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if text != "short content" {
		t.Errorf("the closure rendered %q, want the retry's text", text)
	}
	if fake.calls != 2 {
		t.Fatalf("the fake served %d calls, want the oversized first answer and the shrunken retry", fake.calls)
	}
	if raw, ok := sink.Result(); ok || raw != nil {
		t.Errorf("the sink holds the FAILED first call's payload %s (ok=%t) beside the retry's text — a stale pair the program would read as its answer", raw, ok)
	}
}
