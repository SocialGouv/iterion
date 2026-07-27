package pisdk

import (
	"encoding/json"
	"strconv"
)

// StopReason mirrors pi's StopReason union (packages/ai/src/types.ts).
type StopReason string

const (
	StopPending StopReason = "pending"
	StopStop    StopReason = "stop"
	StopLength  StopReason = "length"
	StopToolUse StopReason = "toolUse"
	StopError   StopReason = "error"
	StopAborted StopReason = "aborted"
)

// Failed reports whether the turn ended in a failure rather than a normal
// stop. This is the ONLY reliable failure signal in `--mode json`: pi's
// print mode writes the error to stderr and exits 1 in text mode, but the
// json branch exits 0 regardless, so the exit code cannot be trusted.
func (s StopReason) Failed() bool { return s == StopError || s == StopAborted }

// Cost is the provider-computed cost breakdown in USD attached to Usage.
type Cost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Total      float64 `json:"total"`
}

// Usage is pi's per-message token and cost accounting.
type Usage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
	// CacheWrite1h is the subset of CacheWrite written with 1h retention.
	// Only Anthropic reports this split.
	CacheWrite1h *int `json:"cacheWrite1h,omitempty"`
	// Reasoning is a SUBSET of Output — Output already includes these
	// tokens. Adding the two would double-count thinking. Left nil by
	// providers that expose no reasoning breakdown.
	Reasoning   *int `json:"reasoning,omitempty"`
	TotalTokens int  `json:"totalTokens"`
	Cost        Cost `json:"cost"`
}

// ReasoningTokens returns Usage.Reasoning or 0 when the provider reports none.
func (u Usage) ReasoningTokens() int {
	if u.Reasoning == nil {
		return 0
	}
	return *u.Reasoning
}

// ContextTokens is the input-side pressure of this message: the prompt plus
// everything served from or written to the cache. It is the quantity a
// context-window gauge tracks, and it is not Usage.Input alone.
func (u Usage) ContextTokens() int { return u.Input + u.CacheRead + u.CacheWrite }

// DiagnosticError is the redacted provider/runtime error attached to a
// Diagnostic. Code carries the upstream HTTP status when the provider
// exposed one, which classifies a rate limit far more reliably than
// pattern-matching the message text.
type DiagnosticError struct {
	Name    string          `json:"name,omitempty"`
	Message string          `json:"message"`
	Stack   string          `json:"stack,omitempty"`
	Code    json.RawMessage `json:"code,omitempty"` // string | number
}

// Diagnostic is one redacted provider/runtime diagnostic on an assistant
// message (failures and recoveries alike).
type Diagnostic struct {
	Type      string           `json:"type"`
	Timestamp int64            `json:"timestamp"`
	Error     *DiagnosticError `json:"error,omitempty"`
	Details   map[string]any   `json:"details,omitempty"`
}

// ContentBlock is one block of a message's content. pi's union
// (TextContent | ThinkingContent | ToolCall | ImageContent) is decoded into a
// single struct: the discriminant is Type and unused fields stay zero.
type ContentBlock struct {
	Type string `json:"type"`

	// Type == "text"
	Text string `json:"text,omitempty"`

	// Type == "thinking"
	Thinking string `json:"thinking,omitempty"`
	Redacted bool   `json:"redacted,omitempty"`

	// Type == "toolCall"
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Arguments map[string]any `json:"arguments,omitempty"`

	// Type == "image"
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

// Message is any entry of a pi conversation: user, assistant, toolResult, or
// one of the app-level custom kinds (bashExecution, branchSummary,
// compactionSummary). Role is the discriminant.
//
// pi models these as a discriminated union; a single struct is used here
// because iterion consumes assistant messages and needs only to recognise
// and skip the rest.
type Message struct {
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content,omitempty"` // string | ContentBlock[]
	Timestamp int64           `json:"timestamp,omitempty"`

	// Role == "assistant"
	API           string       `json:"api,omitempty"`
	Provider      string       `json:"provider,omitempty"`
	Model         string       `json:"model,omitempty"`
	ResponseModel string       `json:"responseModel,omitempty"`
	ResponseID    string       `json:"responseId,omitempty"`
	Diagnostics   []Diagnostic `json:"diagnostics,omitempty"`
	Usage         *Usage       `json:"usage,omitempty"`
	StopReason    StopReason   `json:"stopReason,omitempty"`
	ErrorMessage  string       `json:"errorMessage,omitempty"`

	// Role == "toolResult"
	ToolCallID string `json:"toolCallId,omitempty"`
	ToolName   string `json:"toolName,omitempty"`
	Details    any    `json:"details,omitempty"`
	IsError    bool   `json:"isError,omitempty"`
}

// IsAssistant reports whether this is a model response.
func (m Message) IsAssistant() bool { return m.Role == "assistant" }

// Blocks decodes Content into blocks. pi types a user message's content as
// `string | (TextContent|ImageContent)[]`, so a bare string is returned as a
// single text block.
func (m Message) Blocks() []ContentBlock {
	if len(m.Content) == 0 {
		return nil
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(m.Content, &blocks); err == nil {
		return blocks
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil && s != "" {
		return []ContentBlock{{Type: "text", Text: s}}
	}
	return nil
}

// Text concatenates the message's text blocks, skipping thinking and tool
// calls. This is the assistant's visible answer.
func (m Message) Text() string {
	var out []byte
	for _, b := range m.Blocks() {
		if b.Type == "text" && b.Text != "" {
			out = append(out, b.Text...)
		}
	}
	return string(out)
}

// EffectiveModel is the model that actually served the response.
// ResponseModel is set when it differs from the requested model — a routing
// alias resolving to a concrete model, or a fuzzy pattern matching something
// other than what the caller meant.
func (m Message) EffectiveModel() string {
	if m.ResponseModel != "" {
		return m.ResponseModel
	}
	return m.Model
}

// Identity keys an assistant message for de-duplication. The same message is
// re-emitted across message_update deltas and re-appears in agent_end's
// transcript on a resumed session; summing usage without de-duping multiplies
// the bill by the delta count.
func (m Message) Identity() string {
	if m.ResponseID != "" {
		return m.ResponseID
	}
	// Fall back to the timestamp: unique per message within a session, and
	// present on every message pi emits.
	return strconv.FormatInt(m.Timestamp, 10) + "|" + m.Model
}
