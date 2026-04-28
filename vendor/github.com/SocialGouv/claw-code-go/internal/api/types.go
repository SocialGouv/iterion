package api

// CacheControlMarker is the Anthropic prompt caching marker.
// Set Type to "ephemeral" to enable caching up to this content block.
type CacheControlMarker struct {
	Type string `json:"type"` // "ephemeral"
}

// EphemeralCacheControl returns a cache_control marker with type "ephemeral".
func EphemeralCacheControl() *CacheControlMarker {
	return &CacheControlMarker{Type: "ephemeral"}
}

// ContentBlock represents a single content block in a message.
// Type can be "text", "tool_use", or "tool_result".
type ContentBlock struct {
	Type string `json:"type"`

	// For type == "text"
	Text string `json:"text,omitempty"`

	// For type == "tool_use"
	ID    string         `json:"id,omitempty"`
	Name  string         `json:"name,omitempty"`
	Input map[string]any `json:"input,omitempty"`

	// For type == "tool_result"
	ToolUseID string         `json:"tool_use_id,omitempty"`
	Content   []ContentBlock `json:"content,omitempty"`
	IsError   bool           `json:"is_error,omitempty"`

	// Anthropic prompt caching marker (ignored by non-Anthropic providers).
	CacheControl *CacheControlMarker `json:"cache_control,omitempty"`
}

// ToolResult is a convenience wrapper for building tool_result content blocks.
type ToolResult struct {
	ToolUseID string
	Content   string
	IsError   bool
}

// ToContentBlock converts a ToolResult to a ContentBlock.
func (tr ToolResult) ToContentBlock() ContentBlock {
	cb := ContentBlock{
		Type:      "tool_result",
		ToolUseID: tr.ToolUseID,
		Content: []ContentBlock{
			{Type: "text", Text: tr.Content},
		},
		IsError: tr.IsError,
	}
	return cb
}

// Message represents a single message in the conversation.
type Message struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`

	// IsInjected marks messages that were programmatically injected (e.g., via
	// InjectPrompt) rather than typed by the user. Injected messages are
	// excluded from turn counting in CompactSession and should not contribute
	// to token accounting as real user turns.
	//
	// The field uses omitempty so that existing persisted sessions (which lack
	// the field) deserialize cleanly with IsInjected defaulting to false.
	IsInjected bool `json:"is_injected,omitempty"`
}

// Tool describes a tool that can be called by the model.
type Tool struct {
	Name         string              `json:"name"`
	Description  string              `json:"description"`
	InputSchema  InputSchema         `json:"input_schema"`
	CacheControl *CacheControlMarker `json:"cache_control,omitempty"`
}

// InputSchema is the JSON schema for tool inputs.
type InputSchema struct {
	Type       string              `json:"type"`
	Properties map[string]Property `json:"properties"`
	Required   []string            `json:"required,omitempty"`
}

// Property is a single JSON schema property definition.
//
// Items, Enum and Properties make Property recursive so non-trivial schemas
// (string arrays with `items: {type: "string"}`, enums, nested objects)
// survive a JSON round-trip without losing fields. OpenAI's function-calling
// validator rejects array properties whose `items` is missing, so dropping
// those fields silently produces 400 errors at request time.
type Property struct {
	Type        string              `json:"type"`
	Description string              `json:"description,omitempty"`
	Items       *Property           `json:"items,omitempty"`
	Enum        []any               `json:"enum,omitempty"`
	Properties  map[string]Property `json:"properties,omitempty"`
	Required    []string            `json:"required,omitempty"`
}

// ToolChoice controls which tool the model must use.
// Type can be "auto", "any", or "tool". When Type is "tool", Name must be set.
type ToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// CreateMessageRequest is the request body for /v1/messages.
//
// System vs SystemBlocks: for providers that support prompt caching (Anthropic),
// populate SystemBlocks with ContentBlock entries carrying CacheControl markers.
// The Anthropic client serializes SystemBlocks as the "system" field (array form).
// Non-Anthropic providers (OpenAI) use the plain System string and ignore SystemBlocks.
type CreateMessageRequest struct {
	Model            string         `json:"model"`
	MaxTokens        int            `json:"max_tokens"`
	System           string         `json:"system,omitempty"`
	SystemBlocks     []ContentBlock `json:"-"` // Anthropic array form; takes precedence over System when non-empty
	Messages         []Message      `json:"messages"`
	Tools            []Tool         `json:"tools,omitempty"`
	ToolChoice       *ToolChoice    `json:"tool_choice,omitempty"`
	Stream           bool           `json:"stream"`
	Temperature      *float64       `json:"temperature,omitempty"`
	TopP             *float64       `json:"top_p,omitempty"`
	FrequencyPenalty *float64       `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64       `json:"presence_penalty,omitempty"`
	Stop             []string       `json:"stop,omitempty"`
	ReasoningEffort  string         `json:"reasoning_effort,omitempty"`
}

// --- SSE Event Types ---

// StreamEventType enumerates the SSE event types from the Anthropic API.
type StreamEventType string

const (
	EventMessageStart      StreamEventType = "message_start"
	EventContentBlockStart StreamEventType = "content_block_start"
	EventContentBlockDelta StreamEventType = "content_block_delta"
	EventContentBlockStop  StreamEventType = "content_block_stop"
	EventMessageDelta      StreamEventType = "message_delta"
	EventMessageStop       StreamEventType = "message_stop"
	EventError             StreamEventType = "error"
	EventPing              StreamEventType = "ping"
)

// Delta represents the delta portion of a content_block_delta event.
type Delta struct {
	Type        string `json:"type"`         // "text_delta" or "input_json_delta"
	Text        string `json:"text"`         // for text_delta
	PartialJSON string `json:"partial_json"` // for input_json_delta
}

// MessageDelta is the delta in a message_delta event.
type MessageDelta struct {
	StopReason   string `json:"stop_reason"`
	StopSequence string `json:"stop_sequence"`
}

// UsageDelta contains token usage info.
type UsageDelta struct {
	OutputTokens int `json:"output_tokens"`
}

// ContentBlockInfo holds info about a starting content block.
type ContentBlockInfo struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	ID    string `json:"id"`
	Name  string `json:"name"`
}

// StreamEvent is a parsed SSE event from the Anthropic streaming API.
type StreamEvent struct {
	Type StreamEventType `json:"type"`

	// content_block_delta
	Index int   `json:"index"`
	Delta Delta `json:"delta"`

	// content_block_start
	ContentBlock ContentBlockInfo `json:"content_block"`

	// message_delta
	MessageDelta MessageDelta `json:"delta_message"` // reuse field
	Usage        UsageDelta   `json:"usage"`

	// message_delta stop reason (parsed from "delta" field in message_delta events)
	StopReason string `json:"-"`

	// message_start input token count (parsed from "message.usage.input_tokens")
	InputTokens int `json:"-"`

	// Anthropic prompt cache token counts (parsed from "message.usage")
	CacheCreationInputTokens int `json:"-"`
	CacheReadInputTokens     int `json:"-"`

	// Error
	ErrorMessage string `json:"-"`
}
