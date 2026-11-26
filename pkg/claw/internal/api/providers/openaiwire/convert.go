package openaiwire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
	"github.com/SocialGouv/iterion/pkg/claw/internal/api/httputil"
)

// ConvertMessages maps our Anthropic-style messages to the OpenAI message
// format used by /v1/chat/completions and the Azure deployment proxy.
//
// Key differences from the Anthropic shape:
//   - System prompt → role "system" message prepended.
//   - User tool_result blocks → separate role "tool" messages (one per
//     result), so the model sees them as conversation history.
//   - Assistant tool_use blocks → tool_calls array on the assistant message.
//   - User image blocks → `image_url` parts (a base64 source as a data URL),
//     the user message's content becoming a parts array.
//   - Images inside a tool_result → the tool message keeps the text and
//     announces the images, which travel in the user message that follows
//     the run of tool messages.
//   - A user message right after a tool message is preceded by a bridging
//     assistant message (see toolResultsBridge); one right after another
//     user message joins it.
//
// When system is non-empty it is emitted as the first message; passing ""
// is the supported way to skip it. An image block with neither base64 data
// nor a URL is refused (*api.RequestValidationError): dropping it would send
// the model a turn without the image it was handed.
func ConvertMessages(system string, messages []api.Message) ([]Message, error) {
	var result []Message

	if system != "" {
		result = append(result, Message{Role: "system", Content: StrPtr(system)})
	}

	// toolImages holds the captioned images of the tool results emitted since
	// the last user or assistant message. They wait for the end of the run of
	// tool messages — which may span several input messages, and which a user
	// message in between would break — then ride in the next user message,
	// merged with its text, or alone before the next assistant turn or at the
	// end of the conversation.
	var toolImages []ContentPart
	emitUser := func(parts []ContentPart) {
		parts = CarryToolImages(toolImages, parts, ContentPart{Type: "text", Text: UserContentCaption})
		toolImages = nil
		if len(parts) == 0 {
			return
		}
		if n := len(result); n > 0 && result[n-1].Role == "tool" {
			result = append(result, Message{Role: "assistant", Content: StrPtr(toolResultsBridge)})
		}
		// A user message right after another joins it: the Mistral family's
		// templates refuse user after user, as they refuse user after tool.
		if n := len(result); n > 0 && result[n-1].Role == "user" {
			prev := result[n-1]
			result = result[:n-1]
			if prev.Parts != nil {
				parts = append(append([]ContentPart(nil), prev.Parts...), parts...)
			} else if prev.Content != nil {
				parts = append([]ContentPart{{Type: "text", Text: *prev.Content}}, parts...)
			}
		}
		texts := make([]string, 0, len(parts))
		for _, p := range parts {
			if p.Type != "text" {
				result = append(result, Message{Role: "user", Parts: parts})
				return
			}
			texts = append(texts, p.Text)
		}
		result = append(result, Message{Role: "user", Content: StrPtr(strings.Join(texts, "\n"))})
	}

	for i, msg := range messages {
		switch msg.Role {
		case "user":
			// Tool results become role "tool" messages; text and images
			// go to the user message that follows them.
			var parts []ContentPart
			for _, block := range msg.Content {
				switch block.Type {
				case "text":
					if block.Text != "" {
						parts = append(parts, ContentPart{Type: "text", Text: block.Text})
					}
				case "image":
					part, err := ImagePart(block)
					if err != nil {
						return nil, &api.RequestValidationError{Field: fmt.Sprintf("messages[%d].content", i), Reason: err.Error()}
					}
					parts = append(parts, part)
				case "tool_result":
					content := httputil.ExtractText(block.Content)
					images, err := toolResultImages(block)
					if err != nil {
						return nil, &api.RequestValidationError{Field: fmt.Sprintf("messages[%d].content", i), Reason: fmt.Sprintf("tool result %s: %v", block.ToolUseID, err)}
					}
					if len(images) > 0 {
						content = strings.TrimSpace(content + "\n" + fmt.Sprintf(ToolImageNotice, len(images)))
						toolImages = append(toolImages, ContentPart{Type: "text", Text: fmt.Sprintf(ToolImageCaption, block.ToolUseID)})
						toolImages = append(toolImages, images...)
					}
					result = append(result, Message{
						Role:       "tool",
						Content:    StrPtr(content),
						ToolCallID: block.ToolUseID,
					})
				}
			}
			if len(parts) > 0 {
				emitUser(parts)
			}

		case "assistant":
			emitUser(nil)
			var texts []string
			var toolCalls []ToolCall
			for _, block := range msg.Content {
				switch block.Type {
				case "text":
					if block.Text != "" {
						texts = append(texts, block.Text)
					}
				case "tool_use":
					args, _ := json.Marshal(block.Input)
					toolCalls = append(toolCalls, ToolCall{
						ID:   block.ID,
						Type: "function",
						Function: FunctionCall{
							Name:      block.Name,
							Arguments: string(args),
						},
					})
				}
			}
			var contentPtr *string
			if len(texts) > 0 {
				contentPtr = StrPtr(strings.Join(texts, "\n"))
			}
			result = append(result, Message{
				Role:      "assistant",
				Content:   contentPtr,
				ToolCalls: toolCalls,
			})
		}
	}
	emitUser(nil)

	return result, nil
}

// The wording that carries tool results across the chat protocol. A tool
// message's content is text only, so a tool's images ride in the user
// message that follows; and the chat templates of the Mistral family refuse
// a user message right after a tool message ("Unexpected role 'user' after
// role 'tool'"), text-only or not, so an assistant message bridges the two.
// tool → assistant → user is the one sequence every model measured on a vLLM
// gateway accepted (pixtral, mistral-small/-medium, gemma, qwen, deepseek).
// ToolImageNotice, ToolImageCaption and UserContentCaption are shared with
// the Responses API converter, whose function_call_output is text only as
// well.
const (
	ToolImageNotice    = "[%d image(s) returned by this tool are attached in the next user message]"
	ToolImageCaption   = "Image(s) returned by tool call %s:"
	UserContentCaption = "The user message's own content follows (not returned by a tool):"
	toolResultsBridge  = "Tool results received."
)

// CarryToolImages is the content of the user message that carries the held
// tool images: the captioned tool images first, then the message's own
// parts under their own caption, so an image the user attached is not read
// as the last tool's. Without tool images the parts are returned as they
// are; without parts, the tool images alone.
func CarryToolImages[P any](toolImages, parts []P, caption P) []P {
	if len(toolImages) == 0 {
		return parts
	}
	out := append([]P(nil), toolImages...)
	if len(parts) == 0 {
		return out
	}
	out = append(out, caption)
	return append(out, parts...)
}

// toolResultImages returns the image parts of a tool_result's content.
func toolResultImages(block api.ContentBlock) ([]ContentPart, error) {
	var parts []ContentPart
	for _, c := range block.Content {
		if c.Type != "image" {
			continue
		}
		part, err := ImagePart(c)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	return parts, nil
}

// ImagePart maps an image content block to an `image_url` part.
func ImagePart(block api.ContentBlock) (ContentPart, error) {
	url, err := ImageURLOf(block)
	if err != nil {
		return ContentPart{}, err
	}
	return ContentPart{Type: "image_url", ImageURL: &ImageURL{URL: url}}, nil
}

// ImageURLOf returns the URL an OpenAI-protocol request carries for an image
// block: the source URL, or a data URL built from the base64 source.
func ImageURLOf(block api.ContentBlock) (string, error) {
	src := block.Source
	switch {
	case src == nil:
		return "", fmt.Errorf("image block has no source")
	case src.Type == "url" && src.URL != "":
		return src.URL, nil
	case src.Type == "base64" && src.Data != "" && src.MediaType != "":
		return "data:" + src.MediaType + ";base64," + src.Data, nil
	default:
		return "", fmt.Errorf("image block source %q carries neither a URL nor base64 data with a media type", src.Type)
	}
}

// ConvertTools maps our Tool definitions to OpenAI function-calling format.
// The key difference is that OpenAI uses "parameters" instead of
// "input_schema", and wraps the function descriptor in a {"type":"function"}
// envelope.
//
// The provider name is included in the error message so callers can tell
// which provider's marshal failed when both wrap the same helper.
//
// A marshal failure on any tool's input schema is propagated as an error
// rather than silently dropping the tool — silently skipping a tool causes
// the model to emit tool_use calls for an undeclared name, which derails
// the conversation when the runtime can't dispatch them.
//
// We normalise Go-built schemas before marshalling because OpenAI's function
// validator rejects null for fields it expects as arrays/objects:
//   - omit `required` when nil or empty (a nil []string would marshal
//     as JSON null and OpenAI errors with "None is not of type 'array'")
//   - default `properties` to {} rather than null for tools that
//     declare no parameters (e.g. an MCP `browser_snapshot` taking no
//     input)
//   - default `type` to "object" when empty so the schema parses as a
//     valid JSON-Schema object descriptor.
func ConvertTools(provider string, tools []api.Tool) ([]Tool, error) {
	result := make([]Tool, 0, len(tools))
	for _, t := range tools {
		params, err := NormalizedParameters(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("%s: marshal input schema for tool %q: %w", provider, t.Name, err)
		}
		result = append(result, Tool{
			Type: "function",
			Function: Function{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}
	return result, nil
}

// NormalizedParameters repairs received roots without rebuilding their schema.
// Conforming received schemas pass through verbatim. It normalises the shapes
// OpenAI's validator (both
// /chat/completions and /responses) rejects as `null`:
//   - `required` is OMITTED when nil/empty — a nil []string marshals to JSON
//     null and OpenAI errors "None is not of type 'array'". This is the shape
//     an MCP tool with only optional parameters produces (e.g. firecrawl's
//     firecrawl_interact / firecrawl_monitor_create).
//   - `properties` defaults to `{}` rather than null for a tool that declares
//     no parameters.
//   - `type` defaults to "object" when empty so the schema is a valid
//     JSON-Schema object descriptor.
//
// Both the chat and responses tool converters route through this so the
// normalisation can never drift between the two OpenAI transports.
func NormalizedParameters(schema api.InputSchema) (json.RawMessage, error) {
	if len(schema.Raw) > 0 {
		raw, err := schema.MarshalJSON()
		if err != nil {
			return nil, err
		}
		return normalizeReceivedRoot(raw)
	}
	schemaType := schema.Type
	if schemaType == "" {
		schemaType = "object"
	}
	properties := schema.Properties
	if properties == nil {
		properties = map[string]api.Property{}
	}
	out := map[string]interface{}{
		"type":       schemaType,
		"properties": properties,
	}
	if len(schema.Required) > 0 {
		out["required"] = schema.Required
	}
	// Properties may reference these definitions, including recursively.
	// Keeping only the properties would leave dangling references on both
	// OpenAI transports even though InputSchema preserved the full graph.
	if len(schema.Defs) > 0 {
		out["$defs"] = schema.Defs
	}
	params, err := MarshalJSON(out)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(params), nil
}

// ConvertToolChoice maps our ToolChoice to the chat-completions
// tool_choice field: "auto" / "any" / "none" become "auto" / "required" /
// "none", and "tool" becomes {"type":"function","function":{"name":…}}.
// A nil or unknown choice yields nil, which leaves the field out.
func ConvertToolChoice(tc *api.ToolChoice) any {
	if tc == nil {
		return nil
	}
	switch tc.Type {
	case "tool":
		return map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}}
	case "any":
		return "required"
	case "auto":
		return "auto"
	case "none":
		return "none"
	}
	return nil
}

// normalizeReceivedRoot supplies the OpenAI tool-root defaults AFTER shared
// schema normalization. Null/empty keyword repairs belong to that one helper.
func normalizeReceivedRoot(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, &api.RequestValidationError{Field: "InputSchema", Reason: "root must be a JSON object"}
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	changed := false
	if _, ok := root["type"]; !ok {
		root["type"] = json.RawMessage(`"object"`)
		changed = true
	}
	if _, ok := root["properties"]; !ok {
		root["properties"] = json.RawMessage(`{}`)
		changed = true
	}
	if !changed {
		return raw, nil
	}
	return MarshalJSON(root)
}
