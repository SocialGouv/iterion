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

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/SocialGouv/claw-code-go/internal/api/httputil"
)

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
	// Error is a mid-stream failure frame (`data: {"error":…}`), which an
	// endpoint may send after the 200 status line — as an OpenAI error
	// object or, on some gateways, as a bare string.
	Error json.RawMessage `json:"error,omitempty"`
	// ErrorType is the kind a bare-string error frame names beside it
	// (Hugging Face TGI: `{"error":"…","error_type":"validation"}`).
	ErrorType LooseString `json:"error_type,omitempty"`
}

// LooseString decodes a field endpoints type loosely: a JSON string is
// itself, null is empty, and any other value keeps its JSON text — an odd
// value (a numeric code, an object) never fails the decode of the frame it
// rides on, which would drop the frame.
type LooseString string

// UnmarshalJSON implements json.Unmarshaler.
func (s *LooseString) UnmarshalJSON(b []byte) error {
	var text string
	if json.Unmarshal(b, &text) == nil { // a string, or null: empty
		*s = LooseString(text)
		return nil
	}
	*s = LooseString(strings.TrimSpace(string(b)))
	return nil
}

// ErrorMessage returns the failure a chunk carries, if any. An empty value —
// null, false, "", 0, {}, [] or an object whose fields are all empty —
// carries none. Otherwise the message is the error object's message (with
// its type and code when present), the bare string (with the error_type
// beside it), or the raw value — bounded: the endpoint may not be the
// operator's, and the text travels on into errors and logs.
func (c Chunk) ErrorMessage() (string, bool) {
	msg, ok := errorFrameMessage(c.Error)
	var text string
	if ok && c.ErrorType != "" && json.Unmarshal(c.Error, &text) == nil {
		msg += " (type=" + httputil.TruncateBody(string(c.ErrorType), httputil.FieldTruncateForLog) + ")"
	}
	return msg, ok
}

func errorFrameMessage(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "", false
	}
	var value any
	if err := json.Unmarshal(raw, &value); err == nil && isEmptyJSONValue(value) {
		return "", false
	}
	return describeErrorFrame(raw), true
}

// isEmptyJSONValue reports whether a decoded JSON value carries nothing.
func isEmptyJSONValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case bool:
		return !t
	case string:
		return strings.TrimSpace(t) == ""
	case float64:
		return t == 0
	case []any:
		return len(t) == 0
	case map[string]any:
		for _, field := range t {
			if !isEmptyJSONValue(field) {
				return false
			}
		}
		return true
	}
	return false
}

// describeErrorFrame renders an error value, bounded: the message of an
// error object is cut at httputil.BodyTruncateForLog before its type and code
// are appended, each cut at httputil.FieldTruncateForLog, so a long message
// never costs the caller the verdict and no field unbounds the whole.
func describeErrorFrame(raw []byte) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return httputil.TruncateBody(text, httputil.BodyTruncateForLog)
	}
	var obj struct {
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Code    json.RawMessage `json:"code"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || (obj.Message == "" && obj.Type == "") {
		return httputil.TruncateBody(string(raw), httputil.BodyTruncateForLog)
	}
	msg := httputil.TruncateBody(obj.Message, httputil.BodyTruncateForLog)
	if msg == "" {
		msg = "error frame with no message"
	}
	var detail []string
	if obj.Type != "" {
		detail = append(detail, "type="+httputil.TruncateBody(obj.Type, httputil.FieldTruncateForLog))
	}
	if code := strings.Trim(string(bytes.TrimSpace(obj.Code)), `"`); code != "" && code != "null" {
		detail = append(detail, "code="+httputil.TruncateBody(code, httputil.FieldTruncateForLog))
	}
	if len(detail) > 0 {
		msg += " (" + strings.Join(detail, ", ") + ")"
	}
	return msg
}

// ErrorEventMessage describes the data of an SSE event named "error", which
// is a failure whatever its shape: its error field, else its detail or
// message, else the data itself — bounded like ErrorMessage.
func ErrorEventMessage(data string) string {
	var frame struct {
		Error   json.RawMessage `json:"error"`
		Detail  json.RawMessage `json:"detail"`
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal([]byte(data), &frame); err == nil {
		for _, raw := range []json.RawMessage{frame.Error, frame.Detail} {
			if msg, ok := errorFrameMessage(raw); ok {
				return msg
			}
		}
		// A frame that is itself the error object carries its message at the
		// top level, its type and code beside it. One naming no message
		// keeps its raw text below: whatever key holds the provider's words.
		var top struct {
			Message string `json:"message"`
		}
		if json.Unmarshal([]byte(data), &top) == nil && top.Message != "" {
			return describeErrorFrame([]byte(data))
		}
		if msg, ok := errorFrameMessage(frame.Message); ok {
			return msg
		}
	}
	if strings.TrimSpace(data) == "" {
		return "error event with no data"
	}
	return httputil.TruncateBody(data, httputil.BodyTruncateForLog)
}

// UnparsedFrameError reports the failure carried by a data frame that did
// not decode as a Chunk: its error field when the frame is JSON (an error
// next to a field of an unexpected type), or the frame itself when it is not
// JSON at all — text an endpoint wrote into the stream, which a gateway does
// when it fails to serialise a chunk mid-stream.
func UnparsedFrameError(data string) (string, bool) {
	if !json.Valid([]byte(data)) {
		return httputil.TruncateBody("unparseable data frame: "+data, httputil.BodyTruncateForLog), true
	}
	var frame struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &frame); err != nil {
		return "", false
	}
	return errorFrameMessage(frame.Error)
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
	// counted records that the decoded object named a counter.
	counted bool
}

// Counted reports whether the usage object named a token counter. Some
// endpoints send an empty or partial object ({} or only total_tokens) on
// chunks that report nothing; that is no account of the call.
func (u *Usage) Counted() bool { return u != nil && u.counted }

// UnmarshalJSON decodes the counters and records whether any was present.
func (u *Usage) UnmarshalJSON(b []byte) error {
	var raw struct {
		PromptTokens     *int `json:"prompt_tokens"`
		CompletionTokens *int `json:"completion_tokens"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*u = Usage{counted: raw.PromptTokens != nil || raw.CompletionTokens != nil}
	if raw.PromptTokens != nil {
		u.PromptTokens = *raw.PromptTokens
	}
	if raw.CompletionTokens != nil {
		u.CompletionTokens = *raw.CompletionTokens
	}
	return nil
}

// StrPtr is the trivial helper that returns a pointer to s. It exists so
// builders can write StrPtr("hello") instead of declaring a temporary local
// for every optional field.
func StrPtr(s string) *string { return &s }
