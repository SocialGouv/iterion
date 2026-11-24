package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

// ----- Dispatch decision ----------------------------------------------------

func TestShouldUseResponsesAPI_GatesOnReasoningPlusTools(t *testing.T) {
	tool := api.Tool{
		Name:        "noop",
		Description: "no-op",
		InputSchema: api.InputSchema{Type: "object"},
	}

	tests := []struct {
		name string
		req  api.CreateMessageRequest
		want bool
	}{
		{
			name: "reasoning + tools → responses",
			req: api.CreateMessageRequest{
				Model:           "gpt-5.5",
				ReasoningEffort: "high",
				Tools:           []api.Tool{tool},
			},
			want: true,
		},
		{
			name: "reasoning, no tools → chat completions",
			req: api.CreateMessageRequest{
				Model:           "gpt-5.5",
				ReasoningEffort: "high",
			},
			want: false,
		},
		{
			name: "tools, no reasoning → chat completions",
			req: api.CreateMessageRequest{
				Model: "gpt-4o",
				Tools: []api.Tool{tool},
			},
			want: false,
		},
		{
			name: "neither → chat completions",
			req: api.CreateMessageRequest{
				Model: "gpt-4o",
			},
			want: false,
		},
		{
			name: "empty reasoning + tools → chat completions",
			req: api.CreateMessageRequest{
				Model:           "gpt-5.5",
				ReasoningEffort: "",
				Tools:           []api.Tool{tool},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldUseResponsesAPI(tt.req); got != tt.want {
				t.Errorf("shouldUseResponsesAPI() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ----- Request shape --------------------------------------------------------

func TestBuildResponsesRequest_Shape(t *testing.T) {
	client := &Client{Model: "gpt-5.5", MaxTokens: 1024}
	req := api.CreateMessageRequest{
		Model:           "gpt-5.5",
		MaxTokens:       1024,
		ReasoningEffort: "high",
		System:          "You are concise.",
		Messages: []api.Message{
			{
				Role: "user",
				Content: []api.ContentBlock{
					{Type: "text", Text: "hi"},
				},
			},
		},
		Tools: []api.Tool{
			{
				Name:        "ping",
				Description: "ping the server",
				InputSchema: api.InputSchema{
					Type: "object",
					Properties: map[string]api.Property{
						"target": {Type: "string"},
					},
					Required: []string{"target"},
				},
			},
		},
	}

	got, err := client.buildResponsesRequest(req)
	if err != nil {
		t.Fatalf("buildResponsesRequest: %v", err)
	}

	body, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// instructions replaces system
	if v, _ := payload["instructions"].(string); v != "You are concise." {
		t.Errorf("instructions = %q, want %q", v, "You are concise.")
	}

	// reasoning is an object with effort
	reasoning, ok := payload["reasoning"].(map[string]interface{})
	if !ok {
		t.Fatalf("reasoning missing or wrong shape: %v", payload["reasoning"])
	}
	if reasoning["effort"] != "high" {
		t.Errorf("reasoning.effort = %v, want %q", reasoning["effort"], "high")
	}
	if reasoning["summary"] != "auto" {
		t.Errorf("reasoning.summary = %v, want %q (streamed reasoning summaries)", reasoning["summary"], "auto")
	}

	// tools[0] is FLAT (no nested function object)
	tools, ok := payload["tools"].([]interface{})
	if !ok || len(tools) != 1 {
		t.Fatalf("tools missing: %v", payload["tools"])
	}
	tool := tools[0].(map[string]interface{})
	if tool["type"] != "function" {
		t.Errorf("tool.type = %v, want \"function\"", tool["type"])
	}
	if tool["name"] != "ping" {
		t.Errorf("tool.name = %v, want \"ping\"", tool["name"])
	}
	if _, ok := tool["function"]; ok {
		t.Errorf("responses tool must be flat, found nested .function: %v", tool)
	}

	// stream is true
	if payload["stream"] != true {
		t.Errorf("stream = %v, want true", payload["stream"])
	}

	// max_output_tokens replaces max_tokens / max_completion_tokens
	if _, ok := payload["max_tokens"]; ok {
		t.Errorf("responses request must not contain max_tokens")
	}
	if v, ok := payload["max_output_tokens"].(float64); !ok || int(v) != 1024 {
		t.Errorf("max_output_tokens = %v, want 1024", payload["max_output_tokens"])
	}

	// input is the array form of messages
	input, ok := payload["input"].([]interface{})
	if !ok || len(input) == 0 {
		t.Fatalf("input missing or empty: %v", payload["input"])
	}
}

// ----- End-to-end stream translation via httptest ---------------------------

// TestStreamResponses_TranslatesEvents wires up an httptest server that
// emits a canned /v1/responses SSE stream, then verifies that the
// translator produces the expected sequence of api.StreamEvent values.
func TestStreamResponses_TranslatesEvents(t *testing.T) {
	// A representative event stream: text deltas, then a function call
	// with argument deltas, then completion with usage.
	frames := []string{
		`{"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1"}}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","delta":"Hello"}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","delta":" world"}`,
		`{"type":"response.output_text.done","item_id":"msg_1"}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"ping","status":"in_progress"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"target\":"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"\"alpha\"}"}`,
		`{"type":"response.function_call_arguments.done","item_id":"fc_1","arguments":"{\"target\":\"alpha\"}"}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":10,"output_tokens":7,"total_tokens":17}}}`,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sanity-check the dispatch hit the responses endpoint.
		if r.URL.Path != "/v1/responses" {
			t.Errorf("server got path %q, want /v1/responses", r.URL.Path)
			http.Error(w, "wrong path", http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("missing/invalid Authorization header: %q", got)
		}
		// Inspect the body to confirm `reasoning.effort` and `tools[0].name`
		// were transcribed correctly.
		body, _ := io.ReadAll(r.Body)
		var payload map[string]interface{}
		_ = json.Unmarshal(body, &payload)
		if reasoning, ok := payload["reasoning"].(map[string]interface{}); !ok || reasoning["effort"] != "high" {
			t.Errorf("server saw reasoning = %v, want effort=high", payload["reasoning"])
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	client := &Client{
		APIKey:     "test-key",
		BaseURL:    srv.URL,
		Model:      "gpt-5.5",
		MaxTokens:  256,
		HTTPClient: srv.Client(),
	}

	req := api.CreateMessageRequest{
		Model:           "gpt-5.5",
		MaxTokens:       256,
		System:          "system",
		ReasoningEffort: "high",
		Messages: []api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}},
		},
		Tools: []api.Tool{
			{Name: "ping", Description: "", InputSchema: api.InputSchema{Type: "object"}},
		},
	}

	if !shouldUseResponsesAPI(req) {
		t.Fatalf("dispatch gate failed to fire on reasoning+tools")
	}

	ch, err := client.StreamResponse(context.Background(), req)
	if err != nil {
		t.Fatalf("StreamResponse: %v", err)
	}

	var events []api.StreamEvent
	for ev := range ch {
		events = append(events, ev)
	}

	// Expect a deterministic sequence prefix:
	//   message_start
	//   content_block_start (text, idx 0)
	//   content_block_delta (text_delta "Hello")
	//   content_block_delta (text_delta " world")
	//   content_block_start (tool_use, idx 1, id call_abc, name ping)
	//   content_block_delta (input_json_delta {"target":)
	//   content_block_delta (input_json_delta "alpha"})
	//   content_block_stop (idx 0)         (text closes after stream end)
	//   content_block_stop (idx 1)         (tool closes after stream end)
	//   message_delta (stop_reason tool_use, output tokens 7)
	//   message_stop

	var (
		sawMsgStart     bool
		sawTextStart    bool
		gotTextDeltas   []string
		sawToolStart    bool
		toolID, toolNm  string
		gotToolJSON     []string
		sawTextStop     bool
		sawToolStop     bool
		sawMsgDelta     bool
		gotStopReason   string
		gotOutputTokens int
		sawMsgStop      bool
	)

	for _, ev := range events {
		switch ev.Type {
		case api.EventMessageStart:
			sawMsgStart = true
		case api.EventContentBlockStart:
			switch ev.ContentBlock.Type {
			case "text":
				sawTextStart = true
			case "tool_use":
				sawToolStart = true
				toolID = ev.ContentBlock.ID
				toolNm = ev.ContentBlock.Name
			}
		case api.EventContentBlockDelta:
			switch ev.Delta.Type {
			case "text_delta":
				gotTextDeltas = append(gotTextDeltas, ev.Delta.Text)
			case "input_json_delta":
				gotToolJSON = append(gotToolJSON, ev.Delta.PartialJSON)
			}
		case api.EventContentBlockStop:
			if ev.Index == 0 {
				sawTextStop = true
			} else {
				sawToolStop = true
			}
		case api.EventMessageDelta:
			sawMsgDelta = true
			gotStopReason = ev.StopReason
			gotOutputTokens = ev.Usage.OutputTokens
		case api.EventMessageStop:
			sawMsgStop = true
		}
	}

	if !sawMsgStart {
		t.Error("missing message_start")
	}
	if !sawTextStart {
		t.Error("missing text content_block_start")
	}
	if got := strings.Join(gotTextDeltas, ""); got != "Hello world" {
		t.Errorf("text deltas concatenated = %q, want %q", got, "Hello world")
	}
	if !sawToolStart {
		t.Error("missing tool_use content_block_start")
	}
	if toolID != "call_abc" {
		t.Errorf("tool_use id = %q, want call_abc", toolID)
	}
	if toolNm != "ping" {
		t.Errorf("tool_use name = %q, want ping", toolNm)
	}
	if got := strings.Join(gotToolJSON, ""); got != `{"target":"alpha"}` {
		t.Errorf("tool args concatenated = %q, want %q", got, `{"target":"alpha"}`)
	}
	if !sawTextStop {
		t.Error("missing text content_block_stop")
	}
	if !sawToolStop {
		t.Error("missing tool content_block_stop")
	}
	if !sawMsgDelta {
		t.Error("missing message_delta")
	}
	if gotStopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", gotStopReason)
	}
	if gotOutputTokens != 7 {
		t.Errorf("output tokens = %d, want 7", gotOutputTokens)
	}
	if !sawMsgStop {
		t.Error("missing message_stop")
	}
}

// TestStreamResponses_InterleavedMessageItems exercises the case where a
// /v1/responses stream emits two message items separated by a function
// call: message A → text deltas → function call → message B → text
// deltas → completion.
//
// The translator must allocate distinct block indices for the two
// message items; otherwise deltas from message B would silently
// collapse into message A's block (textBlockIndex was hardcoded to 0).
func TestStreamResponses_InterleavedMessageItems(t *testing.T) {
	frames := []string{
		`{"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`,
		// First message item: text deltas only.
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_A"}}`,
		`{"type":"response.output_text.delta","item_id":"msg_A","delta":"Alpha"}`,
		`{"type":"response.output_text.delta","item_id":"msg_A","delta":"-A"}`,
		`{"type":"response.output_text.done","item_id":"msg_A"}`,
		// Function call between the two message items.
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_x","name":"ping","status":"in_progress"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{}"}`,
		`{"type":"response.function_call_arguments.done","item_id":"fc_1","arguments":"{}"}`,
		// Second message item, post-tool: text deltas must NOT collapse into msg_A's block.
		`{"type":"response.output_item.added","output_index":2,"item":{"type":"message","id":"msg_B"}}`,
		`{"type":"response.output_text.delta","item_id":"msg_B","delta":"Beta"}`,
		`{"type":"response.output_text.delta","item_id":"msg_B","delta":"-B"}`,
		`{"type":"response.output_text.done","item_id":"msg_B"}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":12,"output_tokens":9,"total_tokens":21}}}`,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	client := &Client{
		APIKey:     "test-key",
		BaseURL:    srv.URL,
		Model:      "gpt-5.5",
		MaxTokens:  256,
		HTTPClient: srv.Client(),
	}

	req := api.CreateMessageRequest{
		Model:           "gpt-5.5",
		MaxTokens:       256,
		ReasoningEffort: "high",
		Messages: []api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}},
		},
		Tools: []api.Tool{
			{Name: "ping", Description: "", InputSchema: api.InputSchema{Type: "object"}},
		},
	}

	ch, err := client.StreamResponse(context.Background(), req)
	if err != nil {
		t.Fatalf("StreamResponse: %v", err)
	}

	var events []api.StreamEvent
	for ev := range ch {
		events = append(events, ev)
	}

	// Walk the event stream and bucket text deltas by content_block index.
	// The block index assigned to each text content_block_start is
	// remembered, so we can attribute later deltas to the correct block.
	textStartIndices := []int{}
	textByIndex := map[int]string{}
	toolStartIndex := -1
	stopIndices := []int{}
	for _, ev := range events {
		switch ev.Type {
		case api.EventContentBlockStart:
			switch ev.ContentBlock.Type {
			case "text":
				textStartIndices = append(textStartIndices, ev.Index)
			case "tool_use":
				toolStartIndex = ev.Index
			}
		case api.EventContentBlockDelta:
			if ev.Delta.Type == "text_delta" {
				textByIndex[ev.Index] += ev.Delta.Text
			}
		case api.EventContentBlockStop:
			stopIndices = append(stopIndices, ev.Index)
		}
	}

	if len(textStartIndices) != 2 {
		t.Fatalf("expected 2 distinct text content_block_start events, got %d (indices=%v)",
			len(textStartIndices), textStartIndices)
	}
	idxA := textStartIndices[0]
	idxB := textStartIndices[1]
	if idxA == idxB {
		t.Fatalf("text blocks for msg_A and msg_B share index %d — second message collapsed into first", idxA)
	}
	if toolStartIndex == -1 {
		t.Fatal("missing tool_use content_block_start")
	}
	if toolStartIndex == idxA || toolStartIndex == idxB {
		t.Errorf("tool_use index %d collides with a text block index (A=%d, B=%d)",
			toolStartIndex, idxA, idxB)
	}

	if got := textByIndex[idxA]; got != "Alpha-A" {
		t.Errorf("msg_A text (index %d) = %q, want %q", idxA, got, "Alpha-A")
	}
	if got := textByIndex[idxB]; got != "Beta-B" {
		t.Errorf("msg_B text (index %d) = %q, want %q", idxB, got, "Beta-B")
	}

	// Both text blocks and the tool block should be closed.
	mustStop := map[int]bool{idxA: false, idxB: false, toolStartIndex: false}
	for _, i := range stopIndices {
		if _, ok := mustStop[i]; ok {
			mustStop[i] = true
		}
	}
	for i, ok := range mustStop {
		if !ok {
			t.Errorf("missing content_block_stop for index %d", i)
		}
	}
}

// TestStreamResponses_InterleavedReasoningAndToolCalls covers the
// parallel-reasoning + tool_calls case (CHANGELOG BUGs 2/3): the model
// emits multiple parallel function_calls whose argument deltas
// interleave on the wire, and the translator must keep each tool's
// arguments on its own block index — never mixing deltas across calls.
//
// Frame timeline:
//
//	output_item.added(fc_1, output_index=0)
//	output_item.added(fc_2, output_index=1)
//	fc_1.delta "{"
//	fc_2.delta "{"
//	fc_1.delta "\"a\":1"
//	fc_2.delta "\"b\":2"
//	fc_1.delta "}"
//	fc_2.delta "}"
//	fc_1.done
//	fc_2.done
//	completed
func TestStreamResponses_InterleavedReasoningAndToolCalls(t *testing.T) {
	frames := []string{
		`{"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`,
		// Two parallel function_calls open before any deltas land.
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"alpha","status":"in_progress"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_2","call_id":"call_2","name":"beta","status":"in_progress"}}`,
		// Argument deltas arrive interleaved by item_id.
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_2","delta":"{"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"\"a\":1"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_2","delta":"\"b\":2"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"}"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_2","delta":"}"}`,
		`{"type":"response.function_call_arguments.done","item_id":"fc_1","arguments":"{\"a\":1}"}`,
		`{"type":"response.function_call_arguments.done","item_id":"fc_2","arguments":"{\"b\":2}"}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":8,"output_tokens":12,"total_tokens":20}}}`,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	client := &Client{
		APIKey:     "test-key",
		BaseURL:    srv.URL,
		Model:      "gpt-5.5",
		MaxTokens:  256,
		HTTPClient: srv.Client(),
	}
	req := api.CreateMessageRequest{
		Model:           "gpt-5.5",
		MaxTokens:       256,
		ReasoningEffort: "high",
		Messages: []api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "do both"}}},
		},
		Tools: []api.Tool{
			{Name: "alpha", InputSchema: api.InputSchema{Type: "object"}},
			{Name: "beta", InputSchema: api.InputSchema{Type: "object"}},
		},
	}

	ch, err := client.StreamResponse(context.Background(), req)
	if err != nil {
		t.Fatalf("StreamResponse: %v", err)
	}

	type startInfo struct {
		index int
		id    string
		name  string
	}
	var (
		starts        []startInfo
		argsByIndex   = map[int]string{}
		stoppedIndex  = map[int]bool{}
		gotStopReason string
	)
	for ev := range ch {
		switch ev.Type {
		case api.EventContentBlockStart:
			if ev.ContentBlock.Type == "tool_use" {
				starts = append(starts, startInfo{ev.Index, ev.ContentBlock.ID, ev.ContentBlock.Name})
			}
		case api.EventContentBlockDelta:
			if ev.Delta.Type == "input_json_delta" {
				argsByIndex[ev.Index] += ev.Delta.PartialJSON
			}
		case api.EventContentBlockStop:
			stoppedIndex[ev.Index] = true
		case api.EventMessageDelta:
			gotStopReason = ev.StopReason
		}
	}

	if len(starts) != 2 {
		t.Fatalf("expected 2 tool_use content_block_start events, got %d (%v)", len(starts), starts)
	}
	byID := map[string]startInfo{}
	for _, s := range starts {
		byID[s.id] = s
	}
	one, ok1 := byID["call_1"]
	two, ok2 := byID["call_2"]
	if !ok1 || !ok2 {
		t.Fatalf("missing one of the call ids: starts=%v", starts)
	}
	if one.index == two.index {
		t.Fatalf("two parallel tool_calls share block index %d (call_1 args would mix with call_2)", one.index)
	}
	if one.name != "alpha" || two.name != "beta" {
		t.Errorf("tool names mismatch: call_1=%q (want alpha), call_2=%q (want beta)", one.name, two.name)
	}
	if got, want := argsByIndex[one.index], `{"a":1}`; got != want {
		t.Errorf("call_1 args = %q, want %q (deltas mixed with sibling)", got, want)
	}
	if got, want := argsByIndex[two.index], `{"b":2}`; got != want {
		t.Errorf("call_2 args = %q, want %q (deltas mixed with sibling)", got, want)
	}
	if !stoppedIndex[one.index] {
		t.Errorf("missing content_block_stop for call_1 (index %d)", one.index)
	}
	if !stoppedIndex[two.index] {
		t.Errorf("missing content_block_stop for call_2 (index %d)", two.index)
	}
	if gotStopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", gotStopReason)
	}
}

// TestStreamResponses_TextResumesAfterToolCall covers the case where a
// message item emits text deltas, a function_call item is added in
// parallel, and then the message item resumes with more text deltas
// before its own response.output_text.done arrives.
//
// The translator must keep the message item's text block open across
// the tool-call interruption so the trailing deltas are preserved
// (CHANGELOG BUGs 2/3: best-effort interleave was dropping them).
func TestStreamResponses_TextResumesAfterToolCall(t *testing.T) {
	frames := []string{
		`{"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_A"}}`,
		`{"type":"response.output_text.delta","item_id":"msg_A","delta":"before"}`,
		// Tool call opens while msg_A is still streaming text.
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"ping","status":"in_progress"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{}"}`,
		`{"type":"response.function_call_arguments.done","item_id":"fc_1","arguments":"{}"}`,
		// msg_A resumes text after the tool call lands.
		`{"type":"response.output_text.delta","item_id":"msg_A","delta":"-after"}`,
		`{"type":"response.output_text.done","item_id":"msg_A"}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":4,"output_tokens":6,"total_tokens":10}}}`,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	client := &Client{
		APIKey:     "test-key",
		BaseURL:    srv.URL,
		Model:      "gpt-5.5",
		MaxTokens:  256,
		HTTPClient: srv.Client(),
	}
	req := api.CreateMessageRequest{
		Model:           "gpt-5.5",
		MaxTokens:       256,
		ReasoningEffort: "high",
		Messages: []api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}},
		},
		Tools: []api.Tool{
			{Name: "ping", InputSchema: api.InputSchema{Type: "object"}},
		},
	}

	ch, err := client.StreamResponse(context.Background(), req)
	if err != nil {
		t.Fatalf("StreamResponse: %v", err)
	}

	textByIndex := map[int]string{}
	textStartIndex := -1
	startCount := 0
	for ev := range ch {
		switch ev.Type {
		case api.EventContentBlockStart:
			if ev.ContentBlock.Type == "text" {
				if textStartIndex == -1 {
					textStartIndex = ev.Index
				}
				startCount++
			}
		case api.EventContentBlockDelta:
			if ev.Delta.Type == "text_delta" {
				textByIndex[ev.Index] += ev.Delta.Text
			}
		}
	}

	if startCount != 1 {
		t.Errorf("expected exactly 1 text content_block_start for msg_A, got %d (msg_A's text block was reopened or split)", startCount)
	}
	if got, want := textByIndex[textStartIndex], "before-after"; got != want {
		t.Errorf("msg_A text concatenated = %q, want %q (trailing deltas dropped by premature close)", got, want)
	}
}

// TestStreamResponses_PropagatesAPIError verifies that non-2xx responses
// surface as *api.APIError with status, message, and Retryable populated.
func TestStreamResponses_PropagatesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"bad","type":"invalid_request_error","code":null}}`)
	}))
	defer srv.Close()

	client := &Client{
		APIKey:     "test-key",
		BaseURL:    srv.URL,
		Model:      "gpt-5.5",
		HTTPClient: srv.Client(),
	}

	req := api.CreateMessageRequest{
		Model:           "gpt-5.5",
		ReasoningEffort: "high",
		Tools: []api.Tool{
			{Name: "x", InputSchema: api.InputSchema{Type: "object"}},
		},
	}

	_, err := client.StreamResponse(context.Background(), req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	apiErr, ok := err.(*api.APIError)
	if !ok {
		t.Fatalf("err type = %T, want *api.APIError", err)
	}
	if apiErr.StatusCode != 400 {
		t.Errorf("status = %d, want 400", apiErr.StatusCode)
	}
	if apiErr.Message != "bad" {
		t.Errorf("message = %q, want %q", apiErr.Message, "bad")
	}
	if apiErr.Provider != "openai" {
		t.Errorf("provider = %q, want openai", apiErr.Provider)
	}
}

// TestConvertToolsToResponses_PropagatesMarshalError pins the contract
// that the responses-API tool conversion bubbles up json.Marshal failures
// rather than silently dropping the offending tool. We trigger a marshal
// failure by stuffing a chan into Property.Enum.
func TestConvertToolsToResponses_PropagatesMarshalError(t *testing.T) {
	client := &Client{Model: "gpt-5.5", APIKey: "stub"}
	req := api.CreateMessageRequest{
		Model:           "gpt-5.5",
		MaxTokens:       16,
		ReasoningEffort: "high",
		Messages: []api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}},
		},
		Tools: []api.Tool{
			{
				Name:        "broken",
				Description: "schema with unmarshallable enum",
				InputSchema: api.InputSchema{
					Type: "object",
					Properties: map[string]api.Property{
						"x": {Type: "string", Enum: []any{make(chan int)}},
					},
				},
			},
		},
	}

	if _, err := client.buildResponsesRequest(req); err == nil {
		t.Fatal("expected buildResponsesRequest to fail when a tool's input schema cannot be marshalled")
	} else if !strings.Contains(err.Error(), "broken") {
		t.Errorf("error %q should mention the offending tool name %q", err.Error(), "broken")
	}
}

// TestConvertMessagesToResponsesInput_EmptyToolOutput pins the contract
// that a `function_call_output` item ALWAYS serializes its `output`
// field on the wire — even when the tool produced no text (e.g. a shell
// command that completed silently, or an empty file read).
//
// The OpenAI Responses API rejects the request with
//
//	400: Missing required parameter: 'input[N].output'
//
// when this field is absent. Earlier versions modeled `Output` as a
// `string` with `omitempty`, which stripped the field for empty
// outputs; switching to `*string` keeps `"output": ""` on the wire
// while still omitting the field on item shapes that don't use it.
func TestConvertMessagesToResponsesInput_EmptyToolOutput(t *testing.T) {
	messages := []api.Message{
		{
			Role: "assistant",
			Content: []api.ContentBlock{
				{
					Type:  "tool_use",
					ID:    "call_1",
					Name:  "bash",
					Input: map[string]any{"command": "true"},
				},
			},
		},
		{
			Role: "user",
			Content: []api.ContentBlock{
				api.ToolResult{ToolUseID: "call_1", Content: ""}.ToContentBlock(),
			},
		},
	}

	got := convertMessagesToResponsesInput(messages)

	body, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if !strings.Contains(string(body), `"output":""`) {
		t.Errorf("function_call_output is missing `\"output\":\"\"` on the wire; got %s", body)
	}

	var items []map[string]interface{}
	if err := json.Unmarshal(body, &items); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	var sawCallOutput bool
	for _, it := range items {
		if it["type"] != "function_call_output" {
			continue
		}
		sawCallOutput = true
		out, ok := it["output"]
		if !ok {
			t.Errorf("function_call_output item is missing the `output` field: %v", it)
		}
		if out != "" {
			t.Errorf("output = %v, want \"\" (empty string, present)", out)
		}
		if it["call_id"] != "call_1" {
			t.Errorf("call_id = %v, want \"call_1\"", it["call_id"])
		}
	}
	if !sawCallOutput {
		t.Fatalf("no function_call_output item produced; got items=%v", items)
	}

	// Sanity: a non-empty tool output still round-trips correctly and
	// carries its text in `output`.
	messages[1].Content[0] = api.ToolResult{ToolUseID: "call_1", Content: "ok\n"}.ToContentBlock()
	body, _ = json.Marshal(convertMessagesToResponsesInput(messages))
	if !strings.Contains(string(body), `"output":"ok\n"`) {
		t.Errorf("non-empty tool output failed to round-trip; got %s", body)
	}
}

// TestConvertMessagesToResponsesInput_NoSpuriousOutputField ensures the
// `output` field is NOT emitted on item shapes that don't use it
// (regular role/content turns and `function_call` items). The pointer
// representation lets us keep `omitempty` working for those shapes.
func TestConvertMessagesToResponsesInput_NoSpuriousOutputField(t *testing.T) {
	messages := []api.Message{
		{
			Role:    "user",
			Content: []api.ContentBlock{{Type: "text", Text: "hi"}},
		},
		{
			Role: "assistant",
			Content: []api.ContentBlock{
				{
					Type:  "tool_use",
					ID:    "call_1",
					Name:  "bash",
					Input: map[string]any{"command": "echo"},
				},
			},
		},
	}

	body, err := json.Marshal(convertMessagesToResponsesInput(messages))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var items []map[string]interface{}
	if err := json.Unmarshal(body, &items); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, it := range items {
		typ, _ := it["type"].(string)
		if typ == "function_call_output" {
			t.Fatalf("unexpected function_call_output item: %v", it)
		}
		if _, hasOutput := it["output"]; hasOutput {
			t.Errorf("item type=%q must not carry an `output` field: %v", typ, it)
		}
	}
}

// TestStreamResponses_SurfacesSSEErrorFrame validates that a top-level
// SSE `error` event (emitted by OpenAI for account-level failures
// like insufficient_quota) is surfaced as a typed StreamEvent error
// rather than being silently dropped. Without this handling, claw
// saw a successful response with zero content and reported the
// downstream symptom "model did not produce a structured_output
// tool_use block" — making OpenAI-side billing/quota issues
// indistinguishable from genuine model failures.
func TestStreamResponses_SurfacesSSEErrorFrame(t *testing.T) {
	frames := []string{
		`{"type":"response.created","response":{"id":"resp_x","status":"in_progress"}}`,
		`{"type":"error","error":{"type":"insufficient_quota","code":"insufficient_quota","message":"You exceeded your current quota, please check your plan and billing details."},"sequence_number":2}`,
		`{"type":"response.failed","response":{"id":"resp_x","status":"failed","error":{"code":"insufficient_quota","message":"You exceeded your current quota, please check your plan and billing details."}}}`,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	client := &Client{APIKey: "k", BaseURL: srv.URL, Model: "gpt-5.5", MaxTokens: 256, HTTPClient: srv.Client()}
	req := api.CreateMessageRequest{
		Model: "gpt-5.5", MaxTokens: 256, ReasoningEffort: "high",
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
		Tools:    []api.Tool{{Name: "noop", InputSchema: api.InputSchema{Type: "object"}}},
	}
	ch, err := client.StreamResponse(context.Background(), req)
	if err != nil {
		t.Fatalf("StreamResponse: %v", err)
	}
	var sawError bool
	var errMsg string
	for ev := range ch {
		if ev.Type == api.EventError {
			sawError = true
			errMsg = ev.ErrorMessage
			break
		}
	}
	if !sawError {
		t.Fatalf("expected EventError surfacing the SSE error frame, got none")
	}
	if !strings.Contains(errMsg, "insufficient_quota") {
		t.Errorf("EventError message should contain the OpenAI error code; got %q", errMsg)
	}
}

// TestStreamResponses_SurfacesResponseFailed validates that a
// `response.failed` terminal frame (no preceding top-level error
// event) is also surfaced as EventError. OpenAI sometimes emits
// response.failed without a separate `type=error` frame, depending
// on the failure class.
func TestStreamResponses_SurfacesResponseFailed(t *testing.T) {
	frames := []string{
		`{"type":"response.created","response":{"id":"r","status":"in_progress"}}`,
		`{"type":"response.failed","response":{"id":"r","status":"failed","error":{"code":"server_error","message":"upstream model unavailable"}}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()
	client := &Client{APIKey: "k", BaseURL: srv.URL, Model: "gpt-5.5", MaxTokens: 256, HTTPClient: srv.Client()}
	req := api.CreateMessageRequest{
		Model: "gpt-5.5", MaxTokens: 256, ReasoningEffort: "high",
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
		Tools:    []api.Tool{{Name: "noop", InputSchema: api.InputSchema{Type: "object"}}},
	}
	ch, _ := client.StreamResponse(context.Background(), req)
	var msg string
	for ev := range ch {
		if ev.Type == api.EventError {
			msg = ev.ErrorMessage
			break
		}
	}
	if !strings.Contains(msg, "server_error") {
		t.Errorf("expected response.failed to surface the error code; got %q", msg)
	}
}

// TestStreamResponses_SurfacesResponseIncomplete validates that a
// `response.incomplete` frame (typically max_output_tokens reached
// mid-reasoning) is surfaced as EventError so the caller can
// distinguish "model decided not to respond" from "model was cut
// off". Previously this was silently treated as a successful empty
// turn.
func TestStreamResponses_SurfacesResponseIncomplete(t *testing.T) {
	frames := []string{
		`{"type":"response.created","response":{"id":"r","status":"in_progress"}}`,
		`{"type":"response.incomplete","response":{"id":"r","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":50,"output_tokens":1024,"total_tokens":1074}}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()
	client := &Client{APIKey: "k", BaseURL: srv.URL, Model: "gpt-5.5", MaxTokens: 256, HTTPClient: srv.Client()}
	req := api.CreateMessageRequest{
		Model: "gpt-5.5", MaxTokens: 256, ReasoningEffort: "high",
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
		Tools:    []api.Tool{{Name: "noop", InputSchema: api.InputSchema{Type: "object"}}},
	}
	ch, _ := client.StreamResponse(context.Background(), req)
	var msg string
	for ev := range ch {
		if ev.Type == api.EventError {
			msg = ev.ErrorMessage
			break
		}
	}
	if !strings.Contains(msg, "max_output_tokens") {
		t.Errorf("expected incomplete event to carry the truncation reason; got %q", msg)
	}
}

// TestStreamResponses_PreservesArgsAcrossCompletedReconciliation guards
// against a regression where response.completed unconditionally
// re-invoked MarkStarted for every function_call in Response.Output —
// even those that had already received CallID+Name on
// output_item.added and a full sequence of input_json_delta fragments.
//
// The duplicate ContentBlockStart event on the same block index caused
// the downstream aggregator (iterion's generation.aggregateStream) to
// overwrite the in-flight blockState with a fresh one, dropping the
// accumulated PartialJSON. The tool call then surfaced with an empty
// `input`, producing a "malformed tool input: unexpected end of JSON
// input" tool_error every step and trapping the agent in a sterile
// loop.
//
// Real-world repro: claw-code-go + openai/gpt-5.5 driving an iterion
// `explore`-shaped node against the ChatGPT-Codex backend. Every
// upstream variant emits a final response.completed frame echoing the
// function_call items in Output[], so the reconciliation must be
// idempotent (gated on acc.Started()) rather than blanket-applied.
func TestStreamResponses_PreservesArgsAcrossCompletedReconciliation(t *testing.T) {
	frames := []string{
		`{"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"read_file","status":"in_progress","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"path\":"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"\"/etc/hosts\"}"}`,
		`{"type":"response.function_call_arguments.done","item_id":"fc_1","arguments":"{\"path\":\"/etc/hosts\"}"}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"read_file","status":"completed","arguments":"{\"path\":\"/etc/hosts\"}"}}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[]},{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"read_file","status":"completed","arguments":"{\"path\":\"/etc/hosts\"}"}],"usage":{"input_tokens":4,"output_tokens":12,"total_tokens":16}}}`,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	client := &Client{
		APIKey:     "test-key",
		BaseURL:    srv.URL,
		Model:      "gpt-5.5",
		MaxTokens:  256,
		HTTPClient: srv.Client(),
	}
	req := api.CreateMessageRequest{
		Model:           "gpt-5.5",
		MaxTokens:       256,
		ReasoningEffort: "high",
		Messages: []api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "read /etc/hosts"}}},
		},
		Tools: []api.Tool{
			{Name: "read_file", InputSchema: api.InputSchema{Type: "object"}},
		},
	}

	ch, err := client.StreamResponse(context.Background(), req)
	if err != nil {
		t.Fatalf("StreamResponse: %v", err)
	}

	var (
		toolStartCount  int
		toolStartIndex  = -1
		accumulatedArgs string
	)
	for ev := range ch {
		switch ev.Type {
		case api.EventContentBlockStart:
			if ev.ContentBlock.Type == "tool_use" {
				if toolStartIndex == -1 {
					toolStartIndex = ev.Index
				}
				toolStartCount++
			}
		case api.EventContentBlockDelta:
			if ev.Delta.Type == "input_json_delta" && ev.Index == toolStartIndex {
				accumulatedArgs += ev.Delta.PartialJSON
			}
		}
	}

	if toolStartCount != 1 {
		t.Errorf("expected exactly 1 ContentBlockStart for the function_call, got %d (regression: response.completed reconciliation re-emitted a start event on the same index, prompting the downstream aggregator to wipe the in-flight PartialJSON)", toolStartCount)
	}
	if want := `{"path":"/etc/hosts"}`; accumulatedArgs != want {
		t.Errorf("input_json_delta accumulated args = %q, want %q (regression: a duplicate ContentBlockStart at the same index drops the args collected before it)", accumulatedArgs, want)
	}
}

// streamReasoningFixture serves the frames over an SSE httptest server and
// returns the translated event stream for a reasoning-effort request with
// one declared tool (tools force the Responses API dispatch).
func streamReasoningFixture(t *testing.T, frames []string) <-chan api.StreamEvent {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)

	client := &Client{
		APIKey:     "test-key",
		BaseURL:    srv.URL,
		Model:      "gpt-5.5",
		MaxTokens:  256,
		HTTPClient: srv.Client(),
	}
	req := api.CreateMessageRequest{
		Model:           "gpt-5.5",
		MaxTokens:       256,
		ReasoningEffort: "high",
		Messages: []api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "think hard"}}},
		},
		Tools: []api.Tool{
			{Name: "alpha", InputSchema: api.InputSchema{Type: "object"}},
		},
	}
	ch, err := client.StreamResponse(context.Background(), req)
	if err != nil {
		t.Fatalf("StreamResponse: %v", err)
	}
	return ch
}

// TestStreamResponses_ReasoningSummaryAsThinking verifies that reasoning
// summary events are translated into an Anthropic-style "thinking" content
// block: one block per reasoning item, parts separated by a blank line,
// closed at the item's output_item.done (before the answer text block).
func TestStreamResponses_ReasoningSummaryAsThinking(t *testing.T) {
	frames := []string{
		`{"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}`,
		`{"type":"response.reasoning_summary_part.added","item_id":"rs_1","summary_index":0}`,
		`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","delta":"First "}`,
		`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","delta":"insight."}`,
		`{"type":"response.reasoning_summary_text.done","item_id":"rs_1","summary_index":0}`,
		`{"type":"response.reasoning_summary_part.added","item_id":"rs_1","summary_index":1}`,
		`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","delta":"Second insight."}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_1"}}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","delta":"The answer."}`,
		`{"type":"response.output_text.done","item_id":"msg_1"}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":5,"output_tokens":9,"total_tokens":14}}}`,
	}
	ch := streamReasoningFixture(t, frames)

	var (
		typeByIndex   = map[int]string{}
		thinkingText  string
		answerText    string
		thinkingIdx   = -1
		textIdx       = -1
		thinkingStops int
		order         []string // coarse event order for the stop-before-text assertion
	)
	for ev := range ch {
		switch ev.Type {
		case api.EventContentBlockStart:
			typeByIndex[ev.Index] = ev.ContentBlock.Type
			switch ev.ContentBlock.Type {
			case "thinking":
				thinkingIdx = ev.Index
				order = append(order, "thinking_start")
			case "text":
				textIdx = ev.Index
				order = append(order, "text_start")
			}
		case api.EventContentBlockDelta:
			switch ev.Delta.Type {
			case "thinking_delta":
				thinkingText += ev.Delta.Thinking
			case "text_delta":
				answerText += ev.Delta.Text
			}
		case api.EventContentBlockStop:
			if typeByIndex[ev.Index] == "thinking" {
				thinkingStops++
				order = append(order, "thinking_stop")
			}
		}
	}

	if thinkingIdx < 0 {
		t.Fatal("no thinking content_block_start emitted for the reasoning item")
	}
	if want := "First insight.\n\nSecond insight."; thinkingText != want {
		t.Errorf("thinking text = %q, want %q (parts joined by blank line)", thinkingText, want)
	}
	if thinkingStops != 1 {
		t.Errorf("thinking content_block_stop count = %d, want 1", thinkingStops)
	}
	if textIdx == thinkingIdx {
		t.Errorf("thinking and text share block index %d", textIdx)
	}
	if answerText != "The answer." {
		t.Errorf("answer text = %q, want %q", answerText, "The answer.")
	}
	if want := []string{"thinking_start", "thinking_stop", "text_start"}; fmt.Sprint(order) != fmt.Sprint(want) {
		t.Errorf("event order = %v, want %v (thinking closes at output_item.done, before the answer)", order, want)
	}
}

// TestStreamResponses_ReasoningWithoutSummaryEmitsNoBlock: a reasoning item
// that never produces summary deltas (org not verified, model without
// summaries) must not emit an empty thinking block.
func TestStreamResponses_ReasoningWithoutSummaryEmitsNoBlock(t *testing.T) {
	frames := []string{
		`{"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_1"}}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","delta":"Hi."}`,
		`{"type":"response.output_text.done","item_id":"msg_1"}`,
		`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`,
	}
	for ev := range streamReasoningFixture(t, frames) {
		if ev.Type == api.EventContentBlockStart && ev.ContentBlock.Type == "thinking" {
			t.Fatal("empty thinking block emitted for a summary-less reasoning item")
		}
	}
}
