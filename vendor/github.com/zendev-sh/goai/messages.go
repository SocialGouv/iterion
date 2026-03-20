package goai

import "github.com/zendev-sh/goai/provider"

// SystemMessage creates a system message with text content.
func SystemMessage(text string) provider.Message {
	return provider.Message{
		Role: provider.RoleSystem,
		Content: []provider.Part{
			{Type: provider.PartText, Text: text},
		},
	}
}

// UserMessage creates a user message with text content.
func UserMessage(text string) provider.Message {
	return provider.Message{
		Role: provider.RoleUser,
		Content: []provider.Part{
			{Type: provider.PartText, Text: text},
		},
	}
}

// AssistantMessage creates an assistant message with text content.
func AssistantMessage(text string) provider.Message {
	return provider.Message{
		Role: provider.RoleAssistant,
		Content: []provider.Part{
			{Type: provider.PartText, Text: text},
		},
	}
}

// ToolMessage creates a tool result message.
func ToolMessage(toolCallID, toolName, output string) provider.Message {
	return provider.Message{
		Role: provider.RoleTool,
		Content: []provider.Part{
			{
				Type:       provider.PartToolResult,
				ToolCallID: toolCallID,
				ToolName:   toolName,
				ToolOutput: output,
			},
		},
	}
}
