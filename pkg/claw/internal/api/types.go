package api

import (
	"bytes"
	"encoding/json"
)

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
// Type can be "text", "tool_use", "tool_result", or "image".
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

	// For type == "image" — Anthropic vision content block. Source carries
	// either base64-encoded bytes (Source.Type == "base64") or a URL
	// (Source.Type == "url"). See:
	// https://docs.anthropic.com/en/docs/build-with-claude/vision
	Source *ImageSource `json:"source,omitempty"`

	// Anthropic prompt caching marker (ignored by non-Anthropic providers).
	CacheControl *CacheControlMarker `json:"cache_control,omitempty"`
}

// MarshalJSON enforces the Anthropic protocol invariant that a tool_use
// block carries a present (possibly empty) `input` object, even when
// the LLM produced no arguments — schemas with no required fields
// (e.g. `enter_plan_mode`) and tools the model invokes with no payload
// would otherwise serialise via the default `omitempty` rule, which
// strips empty maps. The Anthropic API then rejects the next turn
// with `messages.N.content.M.tool_use.input: Field required` (HTTP
// 400), aborting the conversation. Forcing `input: {}` for tool_use
// blocks restores round-trip stability without affecting non-tool_use
// blocks (text / tool_result / image still omit input).
//
// We can't simply mutate b.Input to map[string]any{} and forward
// through a struct alias — Go's json encoder treats both nil AND
// empty-map as "omit" under the omitempty tag. The fix is to route
// tool_use blocks through a dedicated struct that drops omitempty
// from Input.
func (b ContentBlock) MarshalJSON() ([]byte, error) {
	switch b.Type {
	case "tool_use":
		type toolUseEcho struct {
			Type         string              `json:"type"`
			ID           string              `json:"id,omitempty"`
			Name         string              `json:"name,omitempty"`
			Input        map[string]any      `json:"input"`
			CacheControl *CacheControlMarker `json:"cache_control,omitempty"`
		}
		input := b.Input
		if input == nil {
			input = map[string]any{}
		}
		return json.Marshal(toolUseEcho{
			Type:         b.Type,
			ID:           b.ID,
			Name:         b.Name,
			Input:        input,
			CacheControl: b.CacheControl,
		})
	case "text":
		// Anthropic requires a present `text` field on every text block.
		// An empty payload — most commonly a tool that produced no output
		// (a `grep` with no matches, a silent `git add`), whose result is
		// spliced into a tool_result as a nested {type:"text", text:""}
		// block — would otherwise serialise via the `omitempty` rule to a
		// text block with no `text` key. The API then rejects the next
		// turn with `messages.N.content.M.tool_result.content.0.text.text:
		// Field required` (HTTP 400), aborting the conversation. Force the
		// field present (empty string is accepted; only omission is not),
		// mirroring the tool_use.input fix above.
		type textEcho struct {
			Type         string              `json:"type"`
			Text         string              `json:"text"`
			CacheControl *CacheControlMarker `json:"cache_control,omitempty"`
		}
		return json.Marshal(textEcho{
			Type:         b.Type,
			Text:         b.Text,
			CacheControl: b.CacheControl,
		})
	default:
		type alias ContentBlock
		return json.Marshal(alias(b))
	}
}

// ImageSource represents the "source" object on an Anthropic image content
// block. Two variants are supported:
//
//   - Type == "base64" → MediaType + Data are required, URL must be empty.
//   - Type == "url"    → URL is required, MediaType + Data must be empty.
//
// MediaType is one of "image/png", "image/jpeg", "image/gif", "image/webp".
// All fields use omitempty so the zero value of a non-image ContentBlock
// continues to serialize identically to pre-vision builds.
type ImageSource struct {
	Type      string `json:"type,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
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

// InputSchema is the JSON schema for tool inputs. Decoded schemas keep Raw
// as the source of truth so keywords unknown to the typed view survive.
// To change a decoded schema, edit Raw or decode a replacement; clearing Raw
// explicitly opts back into serialization of the typed fields.
type InputSchema struct {
	Type       string              `json:"type"`
	Properties map[string]Property `json:"properties"`
	Required   []string            `json:"required,omitempty"`
	// Defs carries the root definitions referenced by named properties.
	// References remain JSON pointers on the wire, including recursive ones;
	// the carrier does not resolve or expand the schema graph.
	Defs       map[string]Property `json:"$defs,omitempty"`
	Raw        json.RawMessage     `json:"-"`
	cleanedRaw string
}

// UnmarshalJSON owns the normalized raw schema and its typed read view.
func (s *InputSchema) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil
	}
	v, raw, err := normalizedSchemaJSON(data)
	if err != nil {
		return err
	}
	view := propertyView(v)
	var defs map[string]Property
	if m, ok := v.(map[string]any); ok {
		defs = propertyMapView(m["$defs"])
	}
	*s = InputSchema{Type: view.Type, Properties: view.Properties, Required: view.Required, Defs: defs, Raw: bytes.Clone(raw), cleanedRaw: string(raw)}
	return nil
}

// MarshalJSON preserves received schemas; Go literals keep their typed shape.
func (s InputSchema) MarshalJSON() ([]byte, error) {
	if len(s.Raw) > 0 {
		if string(s.Raw) == s.cleanedRaw {
			return s.Raw, nil
		}
		return cleanSchemaJSON(s.Raw)
	}
	type plain InputSchema
	raw, err := json.Marshal(plain(s))
	if err != nil {
		return nil, err
	}
	return cleanSchemaJSON(raw)
}

// Property is a single JSON schema property definition.
//
// Items, Enum and Properties make Property recursive so non-trivial schemas
// (string arrays with `items: {type: "string"}`, enums, nested objects)
// survive a JSON round-trip without losing fields. OpenAI's function-calling
// validator rejects array properties whose `items` is missing, so dropping
// those fields silently produces 400 errors at request time.
//
// Type is `omitempty` so an "any-value" property (JSON Schema `{}` — a
// permissive shape callers use when the value can be any JSON primitive
// or composite) round-trips as `{}` rather than `{"type":""}`. Empty
// string for `type` is not a valid JSON Schema value, and OpenAI's
// function-schema validator rejects it with HTTP 400 — `” is not valid
// under any of the given schemas`. Anthropic's validator accepted the
// malformed shape, which is why this only surfaced on OpenAI calls.
type Property struct {
	// Ref preserves a reference wherever a property can appear: directly,
	// in array items, or in a root definition. Nil means absent; a present
	// empty URI refers to the current resource and must remain present.
	Ref  *string `json:"$ref,omitempty"`
	Type string  `json:"type,omitempty"`
	// Types keeps a JSON Schema `type` given as an array of alternatives
	// (`["array","null"]`, or the six-kind union a schema generator emits for
	// an untyped field). Type then holds the first non-null alternative so
	// single-type consumers keep working, and the array round-trips on
	// marshal. Empty for the common single-type property.
	Types       []string            `json:"-"`
	Description string              `json:"description,omitempty"`
	Items       *Property           `json:"items,omitempty"`
	Enum        []any               `json:"enum,omitempty"`
	Properties  map[string]Property `json:"properties,omitempty"`
	Required    []string            `json:"required,omitempty"`
	// Raw takes precedence over the typed read view, as on InputSchema.
	// Only the public decode root retains Raw; child accessors are typed views.
	Raw        json.RawMessage `json:"-"`
	cleanedRaw string
}

// UnmarshalJSON normalizes one decoded tree; descendants are typed read views.
func (p *Property) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil
	}
	v, raw, err := normalizedSchemaJSON(data)
	if err != nil {
		return err
	}
	*p = propertyView(v)
	p.Raw = bytes.Clone(raw)
	p.cleanedRaw = string(raw)
	return nil
}

// MarshalJSON preserves decoded properties, including unknown keywords.
func (p Property) MarshalJSON() ([]byte, error) {
	if len(p.Raw) > 0 {
		if string(p.Raw) == p.cleanedRaw {
			return p.Raw, nil
		}
		return cleanSchemaJSON(p.Raw)
	}
	type plain Property
	if len(p.Types) == 0 {
		return json.Marshal(plain(p))
	}
	return json.Marshal(struct {
		plain
		Type []string `json:"type"`
	}{plain(p), p.Types})
}

// ToolChoice controls which tool the model must use.
// Type can be "auto", "any", or "tool". When Type is "tool", Name must be set.
type ToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// ResponseFormat opts into native structured output on OpenAI-compatible
// routes. Type is "text", "json_schema" or the legacy "json_object". Callers choose
// a format supported by their endpoint; no format is enabled by default.
type ResponseFormat struct {
	Type       string                    `json:"type"`
	JSONSchema *ResponseFormatJSONSchema `json:"json_schema,omitempty"`
}

// ResponseFormatJSONSchema keeps the output schema raw so arbitrary JSON
// Schema keywords and numeric precision survive transport. Strict is always
// emitted, including false, so the caller's choice is explicit.
type ResponseFormatJSONSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Strict      bool            `json:"strict"`
	Schema      json.RawMessage `json:"schema"`
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
	// ResponseFormat is opt-in on OpenAI/Foundry chat and maps to text.format
	// on OpenAI Responses. Anthropic-wire providers reject a non-nil value.
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`

	// Thinking opts into extended thinking. nil means "use the model's
	// default" (adaptive for Opus 4.8/4.7/4.6 and Sonnet 4.6, none
	// otherwise). Set Type:"off" to force thinking off on a model that
	// would otherwise default to adaptive. The Anthropic marshaler shapes
	// this per the model's ThinkingMode (e.g. coercing "enabled" to
	// "adaptive" on adaptive-only models to avoid a 400).
	Thinking *ThinkingConfig `json:"thinking,omitempty"`
}

// OutputConfig carries Anthropic's output_config object. Currently only the
// effort level. Anthropic reads effort from output_config.effort; the OpenAI
// and Foundry providers use the top-level reasoning_effort field instead.
// Source: platform.claude.com/docs/en/build-with-claude/effort
type OutputConfig struct {
	Effort string `json:"effort,omitempty"`
}

// ThinkingConfig configures extended thinking.
//   - Type "adaptive": the model decides per-turn whether to think; effort
//     controls depth. Manual budget_tokens is rejected (400) on these models.
//   - Type "enabled": manual thinking with BudgetTokens (Opus 4.5 and earlier).
//   - Type "off": sentinel used by callers to suppress the model default; the
//     marshaler omits the thinking field entirely (never sent on the wire).
//   - Display "summarized" returns readable (summarized) thinking text;
//     "omitted" returns thinking blocks with an empty field (signature only,
//     the encrypted full reasoning). Opus 4.8/4.7 and Sonnet 5 default to
//     "omitted", so the marshaler requests "summarized" on adaptive requests
//     unless the caller (or CLAW_ANTHROPIC_THINKING_DISPLAY) says otherwise.
//
// Source: platform.claude.com/docs/en/build-with-claude/adaptive-thinking
type ThinkingConfig struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
	Display      string `json:"display,omitempty"`
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
	Type        string `json:"type"`         // "text_delta", "input_json_delta", "thinking_delta", "signature_delta"
	Text        string `json:"text"`         // for text_delta
	PartialJSON string `json:"partial_json"` // for input_json_delta
	Thinking    string `json:"thinking"`     // for thinking_delta (extended-thinking content)
	Signature   string `json:"signature"`    // for signature_delta (signs the preceding thinking block)
}

// MessageDelta is the delta in a message_delta event.
type MessageDelta struct {
	StopReason   string `json:"stop_reason"`
	StopSequence string `json:"stop_sequence"`
}

// UsageDelta contains token usage info.
type UsageDelta struct {
	OutputTokens int `json:"output_tokens"`
	// InputTokens is the prompt count for providers that only learn it
	// once the turn is over. Anthropic and Bedrock report theirs on
	// message_start (StreamEvent.InputTokens) and leave this at 0; the
	// OpenAI endpoints have no equivalent frame, since both put the
	// prompt count in the terminal usage payload. A consumer therefore
	// takes whichever of the two is non-zero — never the later one
	// unconditionally, which would zero the Anthropic count.
	InputTokens int `json:"input_tokens"`
	// InputTokensDetails carries the chat-completions breakdown of the
	// prompt count. CachedTokens is prompt_tokens_details.cached_tokens —
	// the tokens the host served from its prompt cache, which providers
	// bill at a discounted rate. 0 when the API omits the breakdown (and a
	// real account of no cache read when it reports the block with 0).
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	// OutputTokensDetails carries Anthropic's exact billed breakdown.
	// ThinkingTokens counts the raw internal reasoning (always the full
	// amount regardless of thinking.display); 0 when the API omits it.
	OutputTokensDetails struct {
		ThinkingTokens int `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
	// Reported is true when the provider reported its usage for the call:
	// a usage object naming at least one counter, whose value — zero
	// included — is then measured (a counter it left out reads 0). False
	// means the counters are no such account: the provider sent no usage
	// (not asked for it, ignored the request, or the stream ended before
	// it), so a consumer metering spend must not book them as a measured
	// zero. On an EventError it is true only when the failure frame itself
	// carried the usage; otherwise the counters are at most a lower bound —
	// on the OpenAI chat wire, what the stream had reported before failing;
	// none for an Anthropic-shape stream, whose input count rides
	// message_start (StreamEvent.InputTokens) and earlier message_delta
	// usage the consumer has already seen.
	Reported bool `json:"reported,omitempty"`
}

// DecodeUsageDelta decodes an Anthropic-shape usage object (message_delta's
// "usage"). The counters are Reported when the object carries one of them
// as a number; an object without any, or a value that does not decode, is
// no account of the call.
func DecodeUsageDelta(raw json.RawMessage) UsageDelta {
	var u UsageDelta
	_ = json.Unmarshal(raw, &u)
	var present struct {
		OutputTokens *int `json:"output_tokens"`
		InputTokens  *int `json:"input_tokens"`
	}
	u.Reported = json.Unmarshal(raw, &present) == nil && (present.OutputTokens != nil || present.InputTokens != nil)
	return u
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
