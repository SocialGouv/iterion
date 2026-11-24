package testutil

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

const (
	ScenarioPrefix = "PARITY_SCENARIO:"
	DefaultModel   = "claude-sonnet-4-6"
)

// Scenario identifies a mock response pattern.
type Scenario int

const (
	ScenarioStreamingText Scenario = iota
	ScenarioReadFileRoundtrip
	ScenarioGrepChunkAssembly
	ScenarioWriteFileAllowed
	ScenarioWriteFileDenied
	ScenarioMultiToolTurnRoundtrip
	ScenarioBashStdoutRoundtrip
	ScenarioBashPermissionPromptApproved
	ScenarioBashPermissionPromptDenied
	ScenarioPluginToolRoundtrip
	ScenarioAutoCompactTriggered
	ScenarioTokenCostReporting
	ScenarioRateLimited429
	ScenarioAuthFailure401
	ScenarioAuthForbidden403
	ScenarioContextWindowExceeded
	ScenarioChunkedSSE
)

var scenarioNames = map[Scenario]string{
	ScenarioStreamingText:                "streaming_text",
	ScenarioReadFileRoundtrip:            "read_file_roundtrip",
	ScenarioGrepChunkAssembly:            "grep_chunk_assembly",
	ScenarioWriteFileAllowed:             "write_file_allowed",
	ScenarioWriteFileDenied:              "write_file_denied",
	ScenarioMultiToolTurnRoundtrip:       "multi_tool_turn_roundtrip",
	ScenarioBashStdoutRoundtrip:          "bash_stdout_roundtrip",
	ScenarioBashPermissionPromptApproved: "bash_permission_prompt_approved",
	ScenarioBashPermissionPromptDenied:   "bash_permission_prompt_denied",
	ScenarioPluginToolRoundtrip:          "plugin_tool_roundtrip",
	ScenarioAutoCompactTriggered:         "auto_compact_triggered",
	ScenarioTokenCostReporting:           "token_cost_reporting",
	ScenarioRateLimited429:               "rate_limited_429",
	ScenarioAuthFailure401:               "auth_failure_401",
	ScenarioAuthForbidden403:             "auth_forbidden_403",
	ScenarioContextWindowExceeded:        "context_window_exceeded",
	ScenarioChunkedSSE:                   "chunked_sse",
}

var scenarioLookup = func() map[string]Scenario {
	m := make(map[string]Scenario, len(scenarioNames))
	for k, v := range scenarioNames {
		m[v] = k
	}
	return m
}()

// Name returns the string identifier for a scenario.
func (s Scenario) Name() string {
	if n, ok := scenarioNames[s]; ok {
		return n
	}
	return fmt.Sprintf("unknown_%d", int(s))
}

// ParseScenario converts a string to a Scenario, returning ok=false if unknown.
func ParseScenario(name string) (Scenario, bool) {
	s, ok := scenarioLookup[strings.TrimSpace(name)]
	return s, ok
}

// CapturedRequest records a single request made to the mock service.
type CapturedRequest struct {
	Method   string            `json:"method"`
	Path     string            `json:"path"`
	Headers  map[string]string `json:"headers"`
	Scenario string            `json:"scenario"`
	Stream   bool              `json:"stream"`
	RawBody  string            `json:"raw_body"`
}

// MockAnthropicService simulates an Anthropic API server for integration tests.
type MockAnthropicService struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests []CapturedRequest
}

// SpawnMockService creates and starts a new mock Anthropic service.
func SpawnMockService() *MockAnthropicService {
	svc := &MockAnthropicService{}
	svc.server = httptest.NewServer(http.HandlerFunc(svc.handleRequest))
	return svc
}

// BaseURL returns the URL of the mock server.
func (s *MockAnthropicService) BaseURL() string {
	return s.server.URL
}

// CapturedRequests returns a copy of all captured requests.
func (s *MockAnthropicService) CapturedRequests() []CapturedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]CapturedRequest, len(s.requests))
	copy(result, s.requests)
	return result
}

// Close shuts down the mock server.
func (s *MockAnthropicService) Close() {
	s.server.Close()
}

// messageRequest is a minimal representation of the Anthropic API request.
type messageRequest struct {
	Model    string           `json:"model"`
	Messages []messageContent `json:"messages"`
	Stream   bool             `json:"stream"`
}

type messageContent struct {
	Role    string        `json:"role"`
	Content []interface{} `json:"content"`
}

func (s *MockAnthropicService) handleRequest(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body failed", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	scenario, ok := detectScenario(body)
	if !ok {
		http.Error(w, "missing parity scenario", http.StatusBadRequest)
		return
	}

	// Handle error scenarios before body parsing.
	switch scenario {
	case ScenarioRateLimited429:
		s.captureRequest(r, body, scenario, false)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"Rate limited. Please retry after 30 seconds."}}`)
		return
	case ScenarioAuthFailure401:
		s.captureRequest(r, body, scenario, false)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"Invalid API key provided."}}`)
		return
	case ScenarioAuthForbidden403:
		s.captureRequest(r, body, scenario, false)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"type":"error","error":{"type":"permission_error","message":"Your API key does not have permission to use this resource."}}`)
		return
	case ScenarioContextWindowExceeded:
		s.captureRequest(r, body, scenario, false)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 200015 tokens > 200000 maximum"}}`)
		return
	}

	var req messageRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	s.captureRequest(r, body, scenario, req.Stream)

	requestID := requestIDFor(scenario)

	if scenario == ScenarioChunkedSSE {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", requestID)
		w.WriteHeader(http.StatusOK)
		flusher, canFlush := w.(http.Flusher)
		events := buildChunkedSSEEvents()
		for _, event := range events {
			fmt.Fprint(w, event)
			if canFlush {
				flusher.Flush()
			}
		}
		return
	}

	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", requestID)
		w.WriteHeader(http.StatusOK)
		sseBody := buildStreamBody(body, scenario)
		fmt.Fprint(w, sseBody)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-Id", requestID)
	respBody := buildMessageResponse(body, scenario)
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, respBody)
}

func (s *MockAnthropicService) captureRequest(r *http.Request, body []byte, scenario Scenario, stream bool) {
	headers := make(map[string]string)
	for key := range r.Header {
		headers[strings.ToLower(key)] = r.Header.Get(key)
	}
	s.mu.Lock()
	s.requests = append(s.requests, CapturedRequest{
		Method:   r.Method,
		Path:     r.URL.Path,
		Headers:  headers,
		Scenario: scenario.Name(),
		Stream:   stream,
		RawBody:  string(body),
	})
	s.mu.Unlock()
}

func detectScenario(rawBody []byte) (Scenario, bool) {
	bodyStr := string(rawBody)
	idx := strings.Index(bodyStr, ScenarioPrefix)
	if idx < 0 {
		return 0, false
	}
	rest := bodyStr[idx+len(ScenarioPrefix):]
	// Extract the scenario name: alphanumeric and underscores only
	var name strings.Builder
	for _, c := range rest {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			name.WriteRune(c)
		} else {
			break
		}
	}
	s, found := ParseScenario(name.String())
	return s, found
}

func requestIDFor(s Scenario) string {
	return "req_" + s.Name()
}

func buildStreamBody(rawBody []byte, scenario Scenario) string {
	switch scenario {
	case ScenarioStreamingText:
		return streamingTextSSE()
	case ScenarioReadFileRoundtrip:
		if hasToolResult(rawBody) {
			content := extractToolResultText(rawBody)
			return finalTextSSE(fmt.Sprintf("read_file roundtrip complete: %s", extractReadContent(content)))
		}
		return toolUseSSE("toolu_read_fixture", "read_file", `{"path":"fixture.txt"}`)
	case ScenarioGrepChunkAssembly:
		if hasToolResult(rawBody) {
			content := extractToolResultText(rawBody)
			return finalTextSSE(fmt.Sprintf("grep_search matched %s occurrences", extractNumMatches(content)))
		}
		return toolUseSSE("toolu_grep_fixture", "grep_search", `{"pattern":"parity","path":"fixture.txt","output_mode":"count"}`)
	case ScenarioWriteFileAllowed:
		if hasToolResult(rawBody) {
			content := extractToolResultText(rawBody)
			return finalTextSSE(fmt.Sprintf("write_file succeeded: %s", extractFilePath(content)))
		}
		return toolUseSSE("toolu_write_allowed", "write_file", `{"path":"generated/output.txt","content":"created by mock service\n"}`)
	case ScenarioWriteFileDenied:
		if hasToolResult(rawBody) {
			content := extractToolResultText(rawBody)
			return finalTextSSE(fmt.Sprintf("write_file denied as expected: %s", content))
		}
		return toolUseSSE("toolu_write_denied", "write_file", `{"path":"generated/denied.txt","content":"should not exist\n"}`)
	case ScenarioBashStdoutRoundtrip:
		if hasToolResult(rawBody) {
			content := extractToolResultText(rawBody)
			return finalTextSSE(fmt.Sprintf("bash completed: %s", extractBashStdout(content)))
		}
		return toolUseSSE("toolu_bash_stdout", "bash", `{"command":"printf 'alpha from bash'","timeout":1000}`)
	case ScenarioBashPermissionPromptApproved:
		if hasToolResult(rawBody) {
			content := extractToolResultText(rawBody)
			return finalTextSSE(fmt.Sprintf("bash approved and executed: %s", extractBashStdout(content)))
		}
		return toolUseSSE("toolu_bash_prompt_allow", "bash", `{"command":"printf 'approved via prompt'","timeout":1000}`)
	case ScenarioBashPermissionPromptDenied:
		if hasToolResult(rawBody) {
			content := extractToolResultText(rawBody)
			return finalTextSSE(fmt.Sprintf("bash denied as expected: %s", content))
		}
		return toolUseSSE("toolu_bash_prompt_deny", "bash", `{"command":"printf 'should not run'","timeout":1000}`)
	case ScenarioPluginToolRoundtrip:
		if hasToolResult(rawBody) {
			content := extractToolResultText(rawBody)
			return finalTextSSE(fmt.Sprintf("plugin tool completed: %s", content))
		}
		return toolUseSSE("toolu_plugin_echo", "plugin_echo", `{"message":"hello from plugin parity"}`)
	case ScenarioMultiToolTurnRoundtrip:
		if hasToolResult(rawBody) {
			content := extractToolResultText(rawBody)
			return finalTextSSE(fmt.Sprintf("multi-tool roundtrip complete: %s", content))
		}
		return multiToolUseSSE()
	case ScenarioAutoCompactTriggered:
		return finalTextSSEWithUsage("auto compact parity complete.", 50000, 200)
	case ScenarioTokenCostReporting:
		return finalTextSSEWithUsage("token cost reporting parity complete.", 1000, 500)
	default:
		return finalTextSSE("unknown scenario")
	}
}

func buildMessageResponse(rawBody []byte, scenario Scenario) string {
	switch scenario {
	case ScenarioStreamingText:
		return textResponseJSON("msg_streaming_text", "Mock streaming says hello from the parity harness.")
	case ScenarioAutoCompactTriggered:
		return textResponseJSONWithUsage("msg_auto_compact_triggered", "auto compact parity complete.", 50000, 200)
	case ScenarioTokenCostReporting:
		return textResponseJSONWithUsage("msg_token_cost_reporting", "token cost reporting parity complete.", 1000, 500)
	default:
		return textResponseJSON("msg_default", "non-streaming response")
	}
}

// hasToolResult checks if the request body contains a tool_result block.
func hasToolResult(rawBody []byte) bool {
	return strings.Contains(string(rawBody), `"tool_result"`)
}

// extractToolResultText extracts the text content from tool_result blocks.
func extractToolResultText(rawBody []byte) string {
	// Simplified: extract text from tool_result content
	bodyStr := string(rawBody)
	idx := strings.Index(bodyStr, `"tool_result"`)
	if idx < 0 {
		return ""
	}
	// Find the text field in the content after tool_result
	rest := bodyStr[idx:]
	textIdx := strings.Index(rest, `"text"`)
	if textIdx < 0 {
		return ""
	}
	rest = rest[textIdx+7:] // skip past `"text":` (with possible quote)
	rest = strings.TrimLeft(rest, " \t\n\r")
	if len(rest) > 0 && rest[0] == '"' {
		rest = rest[1:]
		endIdx := strings.Index(rest, `"`)
		if endIdx >= 0 {
			return rest[:endIdx]
		}
	}
	return ""
}

func extractReadContent(content string) string {
	if content == "" {
		return "<empty>"
	}
	return content
}

func extractNumMatches(content string) string {
	// Extract numeric portion
	for _, word := range strings.Fields(content) {
		if len(word) > 0 && word[0] >= '0' && word[0] <= '9' {
			return word
		}
	}
	return content
}

func extractFilePath(content string) string {
	if content == "" {
		return "<unknown>"
	}
	return content
}

func extractBashStdout(content string) string {
	if content == "" {
		return "<empty>"
	}
	return content
}

// SSE builders

func appendSSE(buf *strings.Builder, eventType string, data interface{}) {
	jsonBytes, _ := json.Marshal(data)
	fmt.Fprintf(buf, "event: %s\ndata: %s\n\n", eventType, string(jsonBytes))
}

func usageJSON(inputTokens, outputTokens int) map[string]interface{} {
	return map[string]interface{}{
		"input_tokens":                inputTokens,
		"cache_creation_input_tokens": 0,
		"cache_read_input_tokens":     0,
		"output_tokens":               outputTokens,
	}
}

func streamingTextSSE() string {
	var buf strings.Builder
	appendSSE(&buf, "message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            "msg_streaming_text",
			"type":          "message",
			"role":          "assistant",
			"content":       []interface{}{},
			"model":         DefaultModel,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         usageJSON(11, 0),
		},
	})
	appendSSE(&buf, "content_block_start", map[string]interface{}{
		"type":          "content_block_start",
		"index":         0,
		"content_block": map[string]interface{}{"type": "text", "text": ""},
	})
	appendSSE(&buf, "content_block_delta", map[string]interface{}{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]interface{}{"type": "text_delta", "text": "Mock streaming "},
	})
	appendSSE(&buf, "content_block_delta", map[string]interface{}{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]interface{}{"type": "text_delta", "text": "says hello from the parity harness."},
	})
	appendSSE(&buf, "content_block_stop", map[string]interface{}{
		"type":  "content_block_stop",
		"index": 0,
	})
	appendSSE(&buf, "message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": usageJSON(11, 8),
	})
	appendSSE(&buf, "message_stop", map[string]interface{}{"type": "message_stop"})
	return buf.String()
}

func toolUseSSE(toolID, toolName, inputJSON string) string {
	var buf strings.Builder
	msgID := fmt.Sprintf("msg_%s", toolID)
	appendSSE(&buf, "message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"content":       []interface{}{},
			"model":         DefaultModel,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         usageJSON(10, 0),
		},
	})
	appendSSE(&buf, "content_block_start", map[string]interface{}{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]interface{}{
			"type":  "tool_use",
			"id":    toolID,
			"name":  toolName,
			"input": map[string]interface{}{},
		},
	})
	appendSSE(&buf, "content_block_delta", map[string]interface{}{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]interface{}{
			"type":         "input_json_delta",
			"partial_json": inputJSON,
		},
	})
	appendSSE(&buf, "content_block_stop", map[string]interface{}{
		"type":  "content_block_stop",
		"index": 0,
	})
	appendSSE(&buf, "message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": "tool_use", "stop_sequence": nil},
		"usage": usageJSON(10, 3),
	})
	appendSSE(&buf, "message_stop", map[string]interface{}{"type": "message_stop"})
	return buf.String()
}

func multiToolUseSSE() string {
	var buf strings.Builder
	appendSSE(&buf, "message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            "msg_toolu_multi_read",
			"type":          "message",
			"role":          "assistant",
			"content":       []interface{}{},
			"model":         DefaultModel,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         usageJSON(10, 0),
		},
	})
	// Tool 1 - read_file
	appendSSE(&buf, "content_block_start", map[string]interface{}{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]interface{}{
			"type": "tool_use", "id": "toolu_multi_read", "name": "read_file", "input": map[string]interface{}{},
		},
	})
	appendSSE(&buf, "content_block_delta", map[string]interface{}{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": `{"path":"fixture.txt"}`},
	})
	appendSSE(&buf, "content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 0})
	// Tool 2 - grep_search
	appendSSE(&buf, "content_block_start", map[string]interface{}{
		"type":  "content_block_start",
		"index": 1,
		"content_block": map[string]interface{}{
			"type": "tool_use", "id": "toolu_multi_grep", "name": "grep_search", "input": map[string]interface{}{},
		},
	})
	appendSSE(&buf, "content_block_delta", map[string]interface{}{
		"type": "content_block_delta", "index": 1,
		"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": `{"pattern":"parity","path":"fixture.txt","output_mode":"count"}`},
	})
	appendSSE(&buf, "content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 1})
	appendSSE(&buf, "message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": "tool_use", "stop_sequence": nil},
		"usage": usageJSON(10, 3),
	})
	appendSSE(&buf, "message_stop", map[string]interface{}{"type": "message_stop"})
	return buf.String()
}

func finalTextSSE(text string) string {
	return finalTextSSEWithUsage(text, 10, 6)
}

func finalTextSSEWithUsage(text string, inputTokens, outputTokens int) string {
	var buf strings.Builder
	appendSSE(&buf, "message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            "msg_final",
			"type":          "message",
			"role":          "assistant",
			"content":       []interface{}{},
			"model":         DefaultModel,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         usageJSON(inputTokens, 0),
		},
	})
	appendSSE(&buf, "content_block_start", map[string]interface{}{
		"type":          "content_block_start",
		"index":         0,
		"content_block": map[string]interface{}{"type": "text", "text": ""},
	})
	appendSSE(&buf, "content_block_delta", map[string]interface{}{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]interface{}{"type": "text_delta", "text": text},
	})
	appendSSE(&buf, "content_block_stop", map[string]interface{}{
		"type":  "content_block_stop",
		"index": 0,
	})
	appendSSE(&buf, "message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": usageJSON(inputTokens, outputTokens),
	})
	appendSSE(&buf, "message_stop", map[string]interface{}{"type": "message_stop"})
	return buf.String()
}

func textResponseJSON(id, text string) string {
	resp := map[string]interface{}{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"content":       []interface{}{map[string]interface{}{"type": "text", "text": text}},
		"model":         DefaultModel,
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage":         usageJSON(10, 6),
	}
	b, _ := json.Marshal(resp)
	return string(b)
}

func textResponseJSONWithUsage(id, text string, inputTokens, outputTokens int) string {
	resp := map[string]interface{}{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"content":       []interface{}{map[string]interface{}{"type": "text", "text": text}},
		"model":         DefaultModel,
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage":         usageJSON(inputTokens, outputTokens),
	}
	b, _ := json.Marshal(resp)
	return string(b)
}

// buildChunkedSSEEvents returns individual SSE events for the chunked SSE scenario,
// simulating slow network delivery by returning separate chunks.
func buildChunkedSSEEvents() []string {
	var events []string
	msg := map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            "msg_chunked_sse",
			"type":          "message",
			"role":          "assistant",
			"content":       []interface{}{},
			"model":         DefaultModel,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         usageJSON(10, 0),
		},
	}
	events = append(events, marshalSSEEvent("message_start", msg))

	events = append(events, marshalSSEEvent("content_block_start", map[string]interface{}{
		"type":          "content_block_start",
		"index":         0,
		"content_block": map[string]interface{}{"type": "text", "text": ""},
	}))

	// Split text across multiple small deltas
	chunks := []string{"Chunk", " one.", " Chunk", " two.", " Chunk", " three."}
	for _, chunk := range chunks {
		events = append(events, marshalSSEEvent("content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]interface{}{"type": "text_delta", "text": chunk},
		}))
	}

	events = append(events, marshalSSEEvent("content_block_stop", map[string]interface{}{
		"type":  "content_block_stop",
		"index": 0,
	}))
	events = append(events, marshalSSEEvent("message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": usageJSON(10, 6),
	}))
	events = append(events, marshalSSEEvent("message_stop", map[string]interface{}{"type": "message_stop"}))
	return events
}

func marshalSSEEvent(eventType string, data interface{}) string {
	jsonBytes, _ := json.Marshal(data)
	return fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, string(jsonBytes))
}
