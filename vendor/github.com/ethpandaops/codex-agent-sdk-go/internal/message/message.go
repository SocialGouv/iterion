package message

import "encoding/json"

// Message represents any message in the conversation.
// Use type assertion or type switch to determine the concrete type.
type Message interface {
	MessageType() string
}

// Compile-time verification that all message types implement Message.
var (
	_ Message = (*UserMessage)(nil)
	_ Message = (*AssistantMessage)(nil)
	_ Message = (*SystemMessage)(nil)
	_ Message = (*TaskStartedMessage)(nil)
	_ Message = (*TaskCompleteMessage)(nil)
	_ Message = (*ThreadRolledBackMessage)(nil)
	_ Message = (*ResultMessage)(nil)
	_ Message = (*StreamEvent)(nil)
)

// UserMessageContent represents content that can be either a string or []ContentBlock.
type UserMessageContent struct {
	text   *string
	blocks []ContentBlock
}

// NewUserMessageContent creates UserMessageContent from a string.
func NewUserMessageContent(text string) UserMessageContent {
	return UserMessageContent{text: &text}
}

// NewUserMessageContentBlocks creates UserMessageContent from blocks.
func NewUserMessageContentBlocks(blocks []ContentBlock) UserMessageContent {
	return UserMessageContent{blocks: blocks}
}

// String returns the string content if it was originally a string.
func (c *UserMessageContent) String() string {
	if c.text != nil {
		return *c.text
	}

	return ""
}

// Blocks returns content as []ContentBlock (normalizes string to TextBlock).
func (c *UserMessageContent) Blocks() []ContentBlock {
	if c.blocks != nil {
		return c.blocks
	}

	if c.text != nil {
		return []ContentBlock{
			&TextBlock{Type: "text", Text: *c.text},
		}
	}

	return nil
}

// IsString returns true if content was originally a string.
func (c *UserMessageContent) IsString() bool {
	return c.text != nil
}

// MarshalJSON implements json.Marshaler.
func (c UserMessageContent) MarshalJSON() ([]byte, error) {
	if c.text != nil {
		return json.Marshal(*c.text)
	}

	return json.Marshal(c.blocks)
}

// UnmarshalJSON implements json.Unmarshaler.
func (c *UserMessageContent) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		c.text = &text
		c.blocks = nil

		return nil
	}

	var rawBlocks []json.RawMessage
	if err := json.Unmarshal(data, &rawBlocks); err != nil {
		return err
	}

	blocks := make([]ContentBlock, 0, len(rawBlocks))

	for _, raw := range rawBlocks {
		block, err := UnmarshalContentBlock(raw)
		if err != nil {
			return err
		}

		blocks = append(blocks, block)
	}

	c.blocks = blocks
	c.text = nil

	return nil
}

// UserMessage represents a message from the user.
//
//nolint:tagliatelle // CLI uses snake_case
type UserMessage struct {
	Type            string             `json:"type"`
	Content         UserMessageContent `json:"content"`
	UUID            *string            `json:"uuid,omitempty"`
	ParentToolUseID *string            `json:"parent_tool_use_id,omitempty"`
	ToolUseResult   map[string]any     `json:"tool_use_result,omitempty"`
}

// MessageType implements the Message interface.
func (m *UserMessage) MessageType() string { return "user" }

// AssistantMessage represents a message from the agent.
//
//nolint:tagliatelle // CLI uses snake_case
type AssistantMessage struct {
	Type            string                 `json:"type"`
	Content         []ContentBlock         `json:"content"`
	Model           string                 `json:"model"`
	ParentToolUseID *string                `json:"parent_tool_use_id,omitempty"`
	Error           *AssistantMessageError `json:"error,omitempty"`
}

// MessageType implements the Message interface.
func (m *AssistantMessage) MessageType() string { return "assistant" }

// AssistantMessageError represents error types from the assistant.
type AssistantMessageError string

const (
	// AssistantMessageErrorAuthFailed indicates authentication failure.
	AssistantMessageErrorAuthFailed AssistantMessageError = "authentication_failed"
	// AssistantMessageErrorBilling indicates a billing error.
	AssistantMessageErrorBilling AssistantMessageError = "billing_error"
	// AssistantMessageErrorRateLimit indicates rate limiting.
	AssistantMessageErrorRateLimit AssistantMessageError = "rate_limit"
	// AssistantMessageErrorInvalidReq indicates an invalid request.
	AssistantMessageErrorInvalidReq AssistantMessageError = "invalid_request"
	// AssistantMessageErrorServer indicates a server error.
	AssistantMessageErrorServer AssistantMessageError = "server_error"
	// AssistantMessageErrorUnknown indicates an unknown error.
	AssistantMessageErrorUnknown AssistantMessageError = "unknown"
)

// SystemMessage represents a system message.
type SystemMessage struct {
	Type    string         `json:"type"`
	Subtype string         `json:"subtype,omitempty"`
	Data    map[string]any `json:"data,omitempty"`
}

// MessageType implements the Message interface.
func (m *SystemMessage) MessageType() string { return "system" }

// TaskStartedMessage is emitted when a Codex turn starts.
//
//nolint:tagliatelle // Codex uses snake_case
type TaskStartedMessage struct {
	SystemMessage
	TurnID                string `json:"turn_id"`
	CollaborationModeKind string `json:"collaboration_mode_kind,omitempty"`
	ModelContextWindow    *int64 `json:"model_context_window,omitempty"`
}

// TaskCompleteMessage is emitted when a Codex turn completes.
//
//nolint:tagliatelle // Codex uses snake_case
type TaskCompleteMessage struct {
	SystemMessage
	TurnID           string  `json:"turn_id"`
	LastAgentMessage *string `json:"last_agent_message,omitempty"`
}

// ThreadRolledBackMessage is emitted when a thread is rolled back.
//
//nolint:tagliatelle // Codex uses snake_case
type ThreadRolledBackMessage struct {
	SystemMessage
	NumTurns int `json:"num_turns"`
}

// ResultMessage represents the final result of a query.
//
//nolint:tagliatelle // CLI uses snake_case
type ResultMessage struct {
	Type             string  `json:"type"`
	Subtype          string  `json:"subtype"`
	IsError          bool    `json:"is_error"`
	SessionID        string  `json:"session_id"`
	Usage            *Usage  `json:"usage,omitempty"`
	Result           *string `json:"result,omitempty"`
	StructuredOutput any     `json:"structured_output,omitempty"`
}

// MessageType implements the Message interface.
func (m *ResultMessage) MessageType() string { return "result" }

// StreamEvent represents a streaming event from the API.
//
//nolint:tagliatelle // CLI uses snake_case
type StreamEvent struct {
	UUID            string         `json:"uuid"`
	SessionID       string         `json:"session_id"`
	Event           map[string]any `json:"event"`
	ParentToolUseID *string        `json:"parent_tool_use_id,omitempty"`
}

// MessageType implements the Message interface.
func (m *StreamEvent) MessageType() string { return "stream_event" }

// Usage contains token usage information.
//
//nolint:tagliatelle // CLI uses snake_case
type Usage struct {
	InputTokens           int `json:"input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
}

// StreamingMessageContent represents the content of a streaming message.
type StreamingMessageContent struct {
	Role    string             `json:"role"`
	Content UserMessageContent `json:"content"`
}

// StreamingMessage represents a message sent via stdin in streaming mode.
//
//nolint:tagliatelle // CLI protocol uses snake_case
type StreamingMessage struct {
	Type            string                  `json:"type"`
	Message         StreamingMessageContent `json:"message"`
	ParentToolUseID *string                 `json:"parent_tool_use_id,omitempty"`
	SessionID       string                  `json:"session_id,omitempty"`
}
