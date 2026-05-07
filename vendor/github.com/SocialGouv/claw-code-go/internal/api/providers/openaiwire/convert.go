package openaiwire

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/SocialGouv/claw-code-go/internal/api"
	"github.com/SocialGouv/claw-code-go/internal/api/httputil"
)

// ConvertMessages maps our Anthropic-style messages to the OpenAI message
// format used by /v1/chat/completions and the Azure deployment proxy.
//
// Key differences from the Anthropic shape:
//   - System prompt → role "system" message prepended.
//   - User tool_result blocks → separate role "tool" messages (one per
//     result), so the model sees them as conversation history.
//   - Assistant tool_use blocks → tool_calls array on the assistant message.
//
// When system is non-empty it is emitted as the first message; passing ""
// is the supported way to skip it.
func ConvertMessages(system string, messages []api.Message) []Message {
	var result []Message

	if system != "" {
		result = append(result, Message{Role: "system", Content: StrPtr(system)})
	}

	for _, msg := range messages {
		switch msg.Role {
		case "user":
			// Split text content from tool results; tool results become
			// role "tool".
			var texts []string
			for _, block := range msg.Content {
				switch block.Type {
				case "text":
					if block.Text != "" {
						texts = append(texts, block.Text)
					}
				case "tool_result":
					content := httputil.ExtractText(block.Content)
					result = append(result, Message{
						Role:       "tool",
						Content:    StrPtr(content),
						ToolCallID: block.ToolUseID,
					})
				}
			}
			if len(texts) > 0 {
				result = append(result, Message{
					Role:    "user",
					Content: StrPtr(strings.Join(texts, "\n")),
				})
			}

		case "assistant":
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

	return result
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
// We normalise the schema before marshalling because OpenAI's function
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
		schemaType := t.InputSchema.Type
		if schemaType == "" {
			schemaType = "object"
		}
		properties := t.InputSchema.Properties
		if properties == nil {
			properties = map[string]api.Property{}
		}
		schema := map[string]interface{}{
			"type":       schemaType,
			"properties": properties,
		}
		if len(t.InputSchema.Required) > 0 {
			schema["required"] = t.InputSchema.Required
		}
		params, err := json.Marshal(schema)
		if err != nil {
			return nil, fmt.Errorf("%s: marshal input schema for tool %q: %w", provider, t.Name, err)
		}
		result = append(result, Tool{
			Type: "function",
			Function: Function{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  json.RawMessage(params),
			},
		})
	}
	return result, nil
}
