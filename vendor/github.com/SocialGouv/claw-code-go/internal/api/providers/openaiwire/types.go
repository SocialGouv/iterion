// Package openaiwire holds the wire-level JSON types shared by every provider
// that speaks the OpenAI Chat Completions protocol. Today that's the openai
// provider (chat completions; the responses-API path is *different*) and the
// foundry provider (Azure OpenAI).
//
// Originally the foundry package duplicated these types verbatim, citing
// "loose coupling". The cost of that coupling — bug fixes to the openai
// translator silently not applying to foundry — turned out to be steeper
// than the benefit. Centralising here keeps both providers in lockstep.
//
// The provider-specific bits (request building, MarshalJSON quirks for
// reasoning models, endpoint URL, auth headers) intentionally stay in each
// provider package. Only the wire-shape types and the shared
// {messages, tools, sse-stream} translators live here.
package openaiwire

import "encoding/json"

// StreamOpts is the OpenAI `stream_options` object. Currently only
// IncludeUsage is wired up because that's all both providers care about.
type StreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

// Message is a single OpenAI chat message. Content is a pointer so an empty
// content (e.g. assistant turns that only contain tool_calls) can be omitted
// from the wire payload.
type Message struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// Tool is the OpenAI function-tool definition envelope. Type is always
// "function".
type Tool struct {
	Type     string   `json:"type"`
	Function Function `json:"function"`
}

// Function is the tool's function descriptor. Parameters is a raw JSON
// schema object so callers can serialise their own InputSchema.
type Function struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolCall is a tool invocation emitted by the assistant in a non-streaming
// response, or assembled from streaming deltas before being echoed back to
// the API in a follow-up turn.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall pairs a function name with its serialised JSON argument
// string (OpenAI sends arguments as a JSON-encoded string, not a nested
// object).
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Chunk is one streaming SSE chunk from /v1/chat/completions. The final
// `[DONE]` sentinel is handled by the SSE reader and never decoded into
// Chunk.
type Chunk struct {
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage"`
}

// Choice is a single completion choice slot inside a Chunk. We only ever
// look at index 0 in practice but the shape mirrors the OpenAI API.
type Choice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

// Delta is the streaming payload for a single Choice. Content carries text
// fragments, ToolCalls carry tool-invocation fragments.
type Delta struct {
	Content   *string         `json:"content"`
	ToolCalls []ToolCallDelta `json:"tool_calls"`
}

// ToolCallDelta is a single fragment of a tool call streamed via SSE.
// Index identifies WHICH tool call inside the response this fragment
// belongs to; the OpenAI API may interleave fragments from different tool
// calls within the same choice when multiple tools are invoked.
type ToolCallDelta struct {
	Index    int           `json:"index"`
	ID       string        `json:"id"`
	Type     string        `json:"type"`
	Function FunctionDelta `json:"function"`
}

// FunctionDelta carries fragments of the function name and arguments. The
// API may split arguments across many deltas to support large structured
// outputs without buffering the entire JSON string in the response.
type FunctionDelta struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Usage is the token-accounting block in the final SSE chunk. include_usage
// must be set on the request for this to be populated.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// StrPtr is the trivial helper that returns a pointer to s. It exists so
// builders can write StrPtr("hello") instead of declaring a temporary local
// for every optional field.
func StrPtr(s string) *string { return &s }
