package claudesdk

import (
	"encoding/json"
	"fmt"
)

// Message is a sealed interface for NDJSON messages from the CLI.
// Concrete types: [SystemMessage], [AssistantMessage], [UserMessage],
// [ResultMessage], [StreamEvent].
type Message interface {
	messageType() string
	sealed()
}

// ResultSubtype identifies the outcome of a result message.
type ResultSubtype string

const (
	ResultSuccess        ResultSubtype = "success"
	ResultErrorMaxTurns  ResultSubtype = "error_max_turns"
	ResultErrorExecution ResultSubtype = "error_during_execution"
	ResultErrorMaxBudget ResultSubtype = "error_max_budget_usd"
)

// Usage holds token counts for a message or result.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// ModelUsage holds per-model token usage and limits.
type ModelUsage struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	ContextWindow   int `json:"context_window"`
	MaxOutputTokens int `json:"max_output_tokens"`
}

// SystemMessage is emitted once at session initialization.
//
// Forward-compat note: claude_code's wire format evolves (e.g.,
// `agents` shipped as a map of metadata in 2.1.0 then became a flat
// array of names by 2.1.104; `mcp_servers`, `tools`, `slash_commands`
// have undergone similar tweaks). We use json.RawMessage for fields
// whose shape varies across CLI versions to avoid hard-failing on
// unmarshal — the SDK keeps surfacing init events even when a new
// CLI tweaks payload structure.
type SystemMessage struct {
	Type              string          `json:"type"`    // "system"
	Subtype           string          `json:"subtype"` // "init"
	SessionID         string          `json:"session_id"`
	UUID              string          `json:"uuid"`
	Cwd               string          `json:"cwd"`
	Model             string          `json:"model"`
	Tools             json.RawMessage `json:"tools"`
	MCPServers        json.RawMessage `json:"mcp_servers"`
	PermissionMode    string          `json:"permissionMode"`
	APIKeySource      string          `json:"apiKeySource"`
	ClaudeCodeVersion string          `json:"claude_code_version"`
	Agents            json.RawMessage `json:"agents"`
}

// MCPServerCount returns the number of MCP servers reported in the
// init payload, regardless of whether the wire format is an array of
// names or an array of {name, status} objects. Returns -1 when the
// raw message is empty or shaped unexpectedly.
func (s *SystemMessage) MCPServerCount() int {
	return jsonArrayLen(s.MCPServers)
}

// ToolCount returns the number of tools reported in the init payload.
// Returns -1 when the raw message is missing.
func (s *SystemMessage) ToolCount() int {
	return jsonArrayLen(s.Tools)
}

// jsonArrayLen unmarshals raw as a JSON array and returns its
// length. Returns -1 if raw is empty or not a JSON array.
func jsonArrayLen(raw json.RawMessage) int {
	if len(raw) == 0 {
		return -1
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return -1
	}
	return len(arr)
}

func (*SystemMessage) messageType() string { return "system" }
func (*SystemMessage) sealed()             {}

// APIMessage wraps the Claude API message within an assistant message.
type APIMessage struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"` // "message"
	Role       string          `json:"role"` // "assistant"
	Model      string          `json:"model"`
	Content    []ContentBlock  `json:"-"`
	RawContent json.RawMessage `json:"content"`
	StopReason *string         `json:"stop_reason"`
	Usage      Usage           `json:"usage"`
}

// UnmarshalJSON handles deserializing APIMessage with typed content blocks.
func (m *APIMessage) UnmarshalJSON(data []byte) error {
	type Alias APIMessage
	var raw struct {
		Alias
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*m = APIMessage(raw.Alias)
	m.RawContent = raw.Content
	if len(raw.Content) > 0 {
		blocks, err := unmarshalContentBlocks(raw.Content)
		if err != nil {
			return err
		}
		m.Content = blocks
	}
	return nil
}

// MarshalJSON serializes APIMessage with its raw content blocks.
func (m *APIMessage) MarshalJSON() ([]byte, error) {
	type Alias APIMessage
	return json.Marshal(&struct {
		*Alias
		Content json.RawMessage `json:"content"`
	}{
		Alias:   (*Alias)(m),
		Content: m.RawContent,
	})
}

// AssistantMessage represents Claude's response.
type AssistantMessage struct {
	Type            string      `json:"type"` // "assistant"
	UUID            string      `json:"uuid"`
	SessionID       string      `json:"session_id"`
	ParentToolUseID *string     `json:"parent_tool_use_id,omitempty"`
	Message         *APIMessage `json:"message"`
}

func (*AssistantMessage) messageType() string { return "assistant" }
func (*AssistantMessage) sealed()             {}

// UserAPIMessage wraps the Claude API message within a user message.
type UserAPIMessage struct {
	Role       string          `json:"role"` // "user"
	Content    []ContentBlock  `json:"-"`
	RawContent json.RawMessage `json:"content"`
}

// UnmarshalJSON handles deserializing UserAPIMessage with typed content blocks.
func (m *UserAPIMessage) UnmarshalJSON(data []byte) error {
	type Alias UserAPIMessage
	var raw struct {
		Alias
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*m = UserAPIMessage(raw.Alias)
	m.RawContent = raw.Content
	if len(raw.Content) > 0 {
		blocks, err := unmarshalContentBlocks(raw.Content)
		if err != nil {
			return err
		}
		m.Content = blocks
	}
	return nil
}

// UserMessage contains tool results sent back to Claude.
type UserMessage struct {
	Type            string          `json:"type"` // "user"
	UUID            string          `json:"uuid"`
	SessionID       string          `json:"session_id"`
	ParentToolUseID *string         `json:"parent_tool_use_id,omitempty"`
	Message         *UserAPIMessage `json:"message"`
}

func (*UserMessage) messageType() string { return "user" }
func (*UserMessage) sealed()             {}

// ResultMessage is the final message indicating query completion.
type ResultMessage struct {
	Type             string                `json:"type"`    // "result"
	Subtype          ResultSubtype         `json:"subtype"` // "success", "error_max_turns", etc.
	SessionID        string                `json:"session_id"`
	IsError          bool                  `json:"is_error"`
	Result           *string               `json:"result,omitempty"`
	NumTurns         int                   `json:"num_turns"`
	DurationMs       int                   `json:"duration_ms"`
	DurationAPIMs    int                   `json:"duration_api_ms"`
	TotalCostUSD     *float64              `json:"total_cost_usd,omitempty"`
	Usage            *Usage                `json:"usage,omitempty"`
	ModelUsage       map[string]ModelUsage `json:"model_usage,omitempty"`
	StructuredOutput any                   `json:"structured_output,omitempty"`
}

func (*ResultMessage) messageType() string { return "result" }
func (*ResultMessage) sealed()             {}

// StreamEvent wraps a raw Claude API streaming event.
// Only emitted when WithIncludePartialMessages is enabled.
type StreamEvent struct {
	Type            string         `json:"type"` // "stream_event"
	UUID            string         `json:"uuid"`
	SessionID       string         `json:"session_id"`
	ParentToolUseID *string        `json:"parent_tool_use_id,omitempty"`
	Event           map[string]any `json:"event"`
}

func (*StreamEvent) messageType() string { return "stream_event" }
func (*StreamEvent) sealed()             {}

// unmarshalMessage dispatches a raw JSON line to the correct Message type.
func unmarshalMessage(data []byte) (Message, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, &ParseError{Raw: data, Err: err}
	}
	var msg Message
	var err error
	switch probe.Type {
	case "system":
		var m SystemMessage
		err = json.Unmarshal(data, &m)
		msg = &m
	case "assistant":
		var m AssistantMessage
		err = json.Unmarshal(data, &m)
		msg = &m
	case "user":
		var m UserMessage
		err = json.Unmarshal(data, &m)
		msg = &m
	case "result":
		var m ResultMessage
		err = json.Unmarshal(data, &m)
		msg = &m
	case "stream_event":
		var m StreamEvent
		err = json.Unmarshal(data, &m)
		msg = &m
	default:
		return nil, &ParseError{Raw: data, Err: fmt.Errorf("unknown message type: %q", probe.Type)}
	}
	if err != nil {
		return nil, &ParseError{Raw: data, Err: err}
	}
	return msg, nil
}
