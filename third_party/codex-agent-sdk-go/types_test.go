package codexsdk

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUserMessage_Creation tests user message creation.
func TestUserMessage_Creation(t *testing.T) {
	msg := &UserMessage{
		Type:    "user",
		Content: NewUserMessageContent("Hello, Claude!"),
	}

	require.Equal(t, "user", msg.Type)
	require.Equal(t, "user", msg.MessageType())
	require.True(t, msg.Content.IsString())
	require.Equal(t, "Hello, Claude!", msg.Content.String())

	blocks := msg.Content.Blocks()
	require.Len(t, blocks, 1)

	textBlock, ok := blocks[0].(*TextBlock)
	require.True(t, ok)
	require.Equal(t, "text", textBlock.Type)
	require.Equal(t, "Hello, Claude!", textBlock.Text)
}

// TestUserMessage_CreationWithBlocks tests user message creation with content blocks.
func TestUserMessage_CreationWithBlocks(t *testing.T) {
	msg := &UserMessage{
		Type: "user",
		Content: NewUserMessageContentBlocks([]ContentBlock{
			&TextBlock{Type: "text", Text: "Hello, Claude!"},
		}),
	}

	require.Equal(t, "user", msg.Type)
	require.Equal(t, "user", msg.MessageType())
	require.False(t, msg.Content.IsString())

	blocks := msg.Content.Blocks()
	require.Len(t, blocks, 1)

	textBlock, ok := blocks[0].(*TextBlock)
	require.True(t, ok)
	require.Equal(t, "text", textBlock.Type)
	require.Equal(t, "Hello, Claude!", textBlock.Text)
}

// TestAssistantMessage_WithTextContent tests assistant message with text content.
func TestAssistantMessage_WithTextContent(t *testing.T) {
	msg := &AssistantMessage{
		Type:  "assistant",
		Model: "claude-3-5-sonnet-20241022",
		Content: []ContentBlock{
			&TextBlock{Type: "text", Text: "Hello! How can I help you?"},
		},
	}

	require.Equal(t, "assistant", msg.Type)
	require.Equal(t, "assistant", msg.MessageType())
	require.Equal(t, "claude-3-5-sonnet-20241022", msg.Model)
	require.Len(t, msg.Content, 1)

	textBlock, ok := msg.Content[0].(*TextBlock)
	require.True(t, ok)
	require.Equal(t, "Hello! How can I help you?", textBlock.Text)
}

// TestAssistantMessage_WithThinkingContent tests assistant message with thinking content.
func TestAssistantMessage_WithThinkingContent(t *testing.T) {
	msg := &AssistantMessage{
		Type:  "assistant",
		Model: "claude-opus-4-5-20251101",
		Content: []ContentBlock{
			&ThinkingBlock{
				Type:      "thinking",
				Thinking:  "Let me think about this problem...",
				Signature: "sig_abc123",
			},
			&TextBlock{Type: "text", Text: "The answer is 42."},
		},
	}

	require.Equal(t, "assistant", msg.MessageType())
	require.Len(t, msg.Content, 2)

	thinkingBlock, ok := msg.Content[0].(*ThinkingBlock)
	require.True(t, ok)
	require.Equal(t, "thinking", thinkingBlock.Type)
	require.Equal(t, "thinking", thinkingBlock.BlockType())
	require.Equal(t, "Let me think about this problem...", thinkingBlock.Thinking)
	require.Equal(t, "sig_abc123", thinkingBlock.Signature)

	textBlock, ok := msg.Content[1].(*TextBlock)
	require.True(t, ok)
	require.Equal(t, "The answer is 42.", textBlock.Text)
}

// TestToolUseBlock_Creation tests tool use block creation.
func TestToolUseBlock_Creation(t *testing.T) {
	block := &ToolUseBlock{
		Type: "tool_use",
		ID:   "tool_abc123",
		Name: "Bash",
		Input: map[string]any{
			"command":     "ls -la",
			"description": "List files",
		},
	}

	require.Equal(t, "tool_use", block.Type)
	require.Equal(t, "tool_use", block.BlockType())
	require.Equal(t, "tool_abc123", block.ID)
	require.Equal(t, "Bash", block.Name)
	require.Equal(t, "ls -la", block.Input["command"])
	require.Equal(t, "List files", block.Input["description"])
}

// TestToolResultBlock_Creation tests tool result block creation.
func TestToolResultBlock_Creation(t *testing.T) {
	block := &ToolResultBlock{
		Type:      "tool_result",
		ToolUseID: "tool_abc123",
		IsError:   false,
		Content: []ContentBlock{
			&TextBlock{Type: "text", Text: "file1.txt\nfile2.txt"},
		},
	}

	require.Equal(t, "tool_result", block.Type)
	require.Equal(t, "tool_result", block.BlockType())
	require.Equal(t, "tool_abc123", block.ToolUseID)
	require.False(t, block.IsError)
	require.Len(t, block.Content, 1)
}

// TestResultMessage_Creation tests result message creation.
func TestResultMessage_Creation(t *testing.T) {
	msg := &ResultMessage{
		Type:      "result",
		Subtype:   "success",
		IsError:   false,
		SessionID: "session_abc123",
		Usage: &Usage{
			InputTokens:           1000,
			OutputTokens:          500,
			CachedInputTokens:     200,
			ReasoningOutputTokens: 50,
		},
	}

	require.Equal(t, "result", msg.Type)
	require.Equal(t, "result", msg.MessageType())
	require.Equal(t, "success", msg.Subtype)
	require.False(t, msg.IsError)
	require.Equal(t, "session_abc123", msg.SessionID)
	require.NotNil(t, msg.Usage)
	require.Equal(t, 1000, msg.Usage.InputTokens)
	require.Equal(t, 500, msg.Usage.OutputTokens)
	require.Equal(t, 200, msg.Usage.CachedInputTokens)
	require.Equal(t, 50, msg.Usage.ReasoningOutputTokens)
}

// TestCodexAgentOptions_DefaultValues tests default option values.
func TestCodexAgentOptions_DefaultValues(t *testing.T) {
	options := &CodexAgentOptions{}

	require.Empty(t, options.SystemPrompt)
	require.Empty(t, options.Model)
	require.Empty(t, options.PermissionMode)
	require.Empty(t, options.Cwd)
	require.Empty(t, options.CliPath)
	require.Nil(t, options.Env)
	require.Nil(t, options.Hooks)
}

// TestCodexAgentOptions_WithTools tests options with tools.
func TestCodexAgentOptions_WithTools(t *testing.T) {
	options := &CodexAgentOptions{
		AllowedTools:    []string{"Bash", "Read"},
		DisallowedTools: []string{"Write"},
	}

	require.Len(t, options.AllowedTools, 2)
	require.Contains(t, options.AllowedTools, "Bash")
	require.Contains(t, options.AllowedTools, "Read")
	require.Len(t, options.DisallowedTools, 1)
	require.Contains(t, options.DisallowedTools, "Write")
}

// TestCodexAgentOptions_WithPermissionMode tests options with permission mode.
func TestCodexAgentOptions_WithPermissionMode(t *testing.T) {
	options := &CodexAgentOptions{
		PermissionMode: string(PermissionModeAcceptEdits),
	}

	require.Equal(t, string(PermissionModeAcceptEdits), options.PermissionMode)
}

// TestCodexAgentOptions_WithSystemPromptString tests options with system prompt string.
func TestCodexAgentOptions_WithSystemPromptString(t *testing.T) {
	options := &CodexAgentOptions{
		SystemPrompt: "You are a helpful coding assistant.",
	}

	require.Equal(t, "You are a helpful coding assistant.", options.SystemPrompt)
	require.Nil(t, options.SystemPromptPreset)
}

// TestCodexAgentOptions_WithSystemPromptPreset tests options with system prompt preset.
func TestCodexAgentOptions_WithSystemPromptPreset(t *testing.T) {
	options := &CodexAgentOptions{
		SystemPromptPreset: &SystemPromptPreset{
			Type:   "preset",
			Preset: "claude_code",
		},
	}

	require.NotNil(t, options.SystemPromptPreset)
	require.Equal(t, "preset", options.SystemPromptPreset.Type)
	require.Equal(t, "claude_code", options.SystemPromptPreset.Preset)
}

// TestCodexAgentOptions_WithSystemPromptPresetAndAppend tests options with system prompt preset and append.
func TestCodexAgentOptions_WithSystemPromptPresetAndAppend(t *testing.T) {
	options := &CodexAgentOptions{
		SystemPromptPreset: &SystemPromptPreset{
			Type:   "preset",
			Preset: "claude_code",
			Append: new("\n\nAdditional instructions here."),
		},
	}

	require.NotNil(t, options.SystemPromptPreset)
	require.Equal(t, "preset", options.SystemPromptPreset.Type)
	require.Equal(t, "claude_code", options.SystemPromptPreset.Preset)
	require.NotNil(t, options.SystemPromptPreset.Append)
	require.Equal(t, "\n\nAdditional instructions here.", *options.SystemPromptPreset.Append)
}

// TestCodexAgentOptions_WithSessionContinuation tests options with session continuation.
func TestCodexAgentOptions_WithSessionContinuation(t *testing.T) {
	options := &CodexAgentOptions{
		Resume:               "session_previous_123",
		ContinueConversation: true,
		ForkSession:          true,
	}

	require.Equal(t, "session_previous_123", options.Resume)
	require.True(t, options.ContinueConversation)
	require.True(t, options.ForkSession)
}

// TestCodexAgentOptions_WithModel tests options with model specification.
func TestCodexAgentOptions_WithModel(t *testing.T) {
	options := &CodexAgentOptions{
		Model: "claude-opus-4-5-20251101",
	}

	require.Equal(t, "claude-opus-4-5-20251101", options.Model)
}
