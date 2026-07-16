package message

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAssistantMessage(t *testing.T) {
	logger := slog.Default()

	tests := []struct {
		name           string
		data           map[string]any
		wantError      bool
		wantParseErr   bool
		wantErrorValue AssistantMessageError
		wantModel      string
		wantContentLen int
		wantToolUseID  *string
	}{
		{
			name: "no error field",
			data: map[string]any{
				"type": "assistant",
				"message": map[string]any{
					"content": []any{
						map[string]any{"type": "text", "text": "hello"},
					},
					"model": "claude-sonnet-4-5-20250514",
				},
			},
			wantError:      false,
			wantModel:      "claude-sonnet-4-5-20250514",
			wantContentLen: 1,
		},
		{
			name: "authentication_failed error",
			data: map[string]any{
				"type": "assistant",
				"message": map[string]any{
					"content": []any{},
					"model":   "claude-sonnet-4-5-20250514",
				},
				"error": "authentication_failed",
			},
			wantError:      true,
			wantErrorValue: AssistantMessageErrorAuthFailed,
			wantModel:      "claude-sonnet-4-5-20250514",
			wantContentLen: 0,
		},
		{
			name: "rate_limit error",
			data: map[string]any{
				"type": "assistant",
				"message": map[string]any{
					"content": []any{},
					"model":   "claude-sonnet-4-5-20250514",
				},
				"error": "rate_limit",
			},
			wantError:      true,
			wantErrorValue: AssistantMessageErrorRateLimit,
			wantModel:      "claude-sonnet-4-5-20250514",
			wantContentLen: 0,
		},
		{
			name: "unknown error",
			data: map[string]any{
				"type": "assistant",
				"message": map[string]any{
					"content": []any{},
					"model":   "claude-sonnet-4-5-20250514",
				},
				"error": "unknown",
			},
			wantError:      true,
			wantErrorValue: AssistantMessageErrorUnknown,
			wantModel:      "claude-sonnet-4-5-20250514",
			wantContentLen: 0,
		},
		{
			name: "error at top level not in nested message",
			data: map[string]any{
				"type": "assistant",
				"message": map[string]any{
					"content": []any{
						map[string]any{"type": "text", "text": "partial response"},
					},
					"model": "claude-sonnet-4-5-20250514",
					"error": "should_be_ignored",
				},
				"error":              "billing_error",
				"parent_tool_use_id": "tool-123",
			},
			wantError:      true,
			wantErrorValue: AssistantMessageErrorBilling,
			wantModel:      "claude-sonnet-4-5-20250514",
			wantContentLen: 1,
			wantToolUseID:  new("tool-123"),
		},
		{
			name: "missing message field returns parse error",
			data: map[string]any{
				"type": "assistant",
			},
			wantParseErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, err := Parse(logger, tt.data)

			if tt.wantParseErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)

			assistant, ok := msg.(*AssistantMessage)
			require.True(t, ok, "expected *AssistantMessage")
			require.Equal(t, "assistant", assistant.Type)
			require.Equal(t, tt.wantModel, assistant.Model)
			require.Len(t, assistant.Content, tt.wantContentLen)

			if tt.wantError {
				require.NotNil(t, assistant.Error)
				require.Equal(t, tt.wantErrorValue, *assistant.Error)
			} else {
				require.Nil(t, assistant.Error)
			}

			if tt.wantToolUseID != nil {
				require.NotNil(t, assistant.ParentToolUseID)
				require.Equal(t, *tt.wantToolUseID, *assistant.ParentToolUseID)
			}
		})
	}
}

func TestParseCodexAgentMessageDeltaSuppression(t *testing.T) {
	logger := slog.Default()

	tests := []struct {
		name       string
		data       map[string]any
		wantType   string
		wantSystem bool
	}{
		{
			name: "item.updated agent_message suppressed to SystemMessage",
			data: map[string]any{
				"type": "item.updated",
				"item": map[string]any{
					"type": "agent_message",
					"text": "partial delta",
				},
			},
			wantType:   "system",
			wantSystem: true,
		},
		{
			name: "item.started agent_message suppressed to SystemMessage",
			data: map[string]any{
				"type": "item.started",
				"item": map[string]any{
					"type": "agent_message",
					"text": "",
				},
			},
			wantType:   "system",
			wantSystem: true,
		},
		{
			name: "empty completed reasoning suppressed to SystemMessage",
			data: map[string]any{
				"type": "item.completed",
				"item": map[string]any{
					"type": "reasoning",
				},
			},
			wantType:   "system",
			wantSystem: true,
		},
		{
			name: "item.completed agent_message emits AssistantMessage",
			data: map[string]any{
				"type": "item.completed",
				"item": map[string]any{
					"type": "agent_message",
					"text": "complete text",
				},
			},
			wantType:   "assistant",
			wantSystem: false,
		},
		{
			name: "item.updated command_execution emits AssistantMessage",
			data: map[string]any{
				"type": "item.updated",
				"item": map[string]any{
					"type":    "command_execution",
					"id":      "cmd_1",
					"command": "ls",
				},
			},
			wantType:   "assistant",
			wantSystem: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, err := Parse(logger, tt.data)
			require.NoError(t, err)
			require.Equal(t, tt.wantType, msg.MessageType())

			if tt.wantSystem {
				sys, ok := msg.(*SystemMessage)
				require.True(t, ok, "expected *SystemMessage")
				require.True(t,
					strings.Contains(sys.Subtype, "agent_message_delta") ||
						strings.Contains(sys.Subtype, "reasoning_delta"),
				)
			}
		})
	}
}

func TestParseCodexReasoningItemWithText(t *testing.T) {
	logger := slog.Default()

	msg, err := Parse(logger, map[string]any{
		"type": "item.completed",
		"item": map[string]any{
			"type": "reasoning",
			"id":   "reason_1",
			"text": "Let me think about this problem step by step.",
		},
	})
	require.NoError(t, err)

	assistant, ok := msg.(*AssistantMessage)
	require.True(t, ok, "reasoning item with text should produce AssistantMessage")
	require.Len(t, assistant.Content, 1)

	thinking, ok := assistant.Content[0].(*ThinkingBlock)
	require.True(t, ok, "expected ThinkingBlock")
	require.Equal(t, BlockTypeThinking, thinking.Type)
	require.Equal(t, "Let me think about this problem step by step.", thinking.Thinking)
}

func TestParseCodexDynamicToolCall(t *testing.T) {
	logger := slog.Default()

	msg, err := Parse(logger, map[string]any{
		"type": "item.completed",
		"item": map[string]any{
			"id":   "call_123",
			"type": "dynamic_tool_call",
			"name": "add",
			"arguments": map[string]any{
				"a": 12.0,
				"b": 30.0,
			},
			"success": true,
			"contentItems": []any{
				map[string]any{
					"type": "inputText",
					"text": "{\"result\":42}",
				},
			},
		},
	})
	require.NoError(t, err)

	assistant, ok := msg.(*AssistantMessage)
	require.True(t, ok, "expected *AssistantMessage")
	require.Len(t, assistant.Content, 2)

	toolUse, ok := assistant.Content[0].(*ToolUseBlock)
	require.True(t, ok, "expected ToolUseBlock")
	require.Equal(t, "add", toolUse.Name)
	require.Equal(t, 12.0, toolUse.Input["a"])
	require.Equal(t, 30.0, toolUse.Input["b"])

	toolResult, ok := assistant.Content[1].(*ToolResultBlock)
	require.True(t, ok, "expected ToolResultBlock")
	require.Equal(t, "call_123", toolResult.ToolUseID)
	require.False(t, toolResult.IsError)
	require.Len(t, toolResult.Content, 1)

	textBlock, ok := toolResult.Content[0].(*TextBlock)
	require.True(t, ok, "expected TextBlock")
	require.Equal(t, "{\"result\":42}", textBlock.Text)
}

func TestParseCodexDynamicToolCall_NonTextContentNotDropped(t *testing.T) {
	logger := slog.Default()

	msg, err := Parse(logger, map[string]any{
		"type": "item.completed",
		"item": map[string]any{
			"id":   "call_image_123",
			"type": "dynamic_tool_call",
			"name": "render_diagram",
			"arguments": map[string]any{
				"prompt": "draw a diagram",
			},
			"success": true,
			"contentItems": []any{
				map[string]any{
					"type":     "image",
					"data":     "ZmFrZV9wbmc=",
					"mimeType": "image/png",
				},
			},
		},
	})
	require.NoError(t, err)

	assistant, ok := msg.(*AssistantMessage)
	require.True(t, ok, "expected *AssistantMessage")
	require.Len(t, assistant.Content, 2)

	toolResult, ok := assistant.Content[1].(*ToolResultBlock)
	require.True(t, ok, "expected ToolResultBlock")
	require.NotEmpty(t,
		toolResult.Content,
		"non-text dynamic tool results should be preserved instead of being dropped",
	)
}

func TestParseCodexDynamicToolCall_PublicSDKMCPNameMatchesMCPToolCallFormat(t *testing.T) {
	logger := slog.Default()

	regularMsg, err := Parse(logger, map[string]any{
		"type": "item.completed",
		"item": map[string]any{
			"id":     "mcp_123",
			"type":   "mcp_tool_call",
			"server": "calc",
			"tool":   "add",
		},
	})
	require.NoError(t, err)

	sdkMsg, err := Parse(logger, map[string]any{
		"type": "item.completed",
		"item": map[string]any{
			"id":   "sdk_123",
			"type": "dynamic_tool_call",
			"tool": "mcp__calc__add",
			"arguments": map[string]any{
				"a": 15.0,
				"b": 27.0,
			},
			"success": true,
		},
	})
	require.NoError(t, err)

	regularAssistant, ok := regularMsg.(*AssistantMessage)
	require.True(t, ok, "expected *AssistantMessage for MCP tool call")

	sdkAssistant, ok := sdkMsg.(*AssistantMessage)
	require.True(t, ok, "expected *AssistantMessage for SDK-backed MCP tool call")

	regularToolUse, ok := regularAssistant.Content[0].(*ToolUseBlock)
	require.True(t, ok, "expected ToolUseBlock for MCP tool call")

	sdkToolUse, ok := sdkAssistant.Content[0].(*ToolUseBlock)
	require.True(t, ok, "expected ToolUseBlock for SDK-backed MCP tool call")

	require.Equal(t,
		regularToolUse.Name,
		sdkToolUse.Name,
		"SDK-backed MCP tools should expose the same public tool name format as normal MCP tool calls",
	)
}

func TestParseCodexTurnCompletedStructuredOutput(t *testing.T) {
	logger := slog.Default()

	msg, err := Parse(logger, map[string]any{
		"type": "turn.completed",
		"structured_output": map[string]any{
			"answer": "4",
		},
	})
	require.NoError(t, err)

	result, ok := msg.(*ResultMessage)
	require.True(t, ok, "expected *ResultMessage")

	structured, ok := result.StructuredOutput.(map[string]any)
	require.True(t, ok, "expected structured output map")
	require.Equal(t, "4", structured["answer"])
}

func TestParseCodexFileChangeKindObject(t *testing.T) {
	logger := slog.Default()

	data := map[string]any{
		"type": "item.completed",
		"item": map[string]any{
			"id":   "item-1",
			"type": "file_change",
			"changes": []any{
				map[string]any{
					"path": "hello.txt",
					"kind": map[string]any{
						"type": "create",
					},
				},
			},
		},
	}

	msg, err := Parse(logger, data)
	require.NoError(t, err)

	assistant, ok := msg.(*AssistantMessage)
	require.True(t, ok, "expected *AssistantMessage")
	require.Len(t, assistant.Content, 1)

	toolUse, ok := assistant.Content[0].(*ToolUseBlock)
	require.True(t, ok, "expected first content block to be ToolUseBlock")
	require.Equal(t, "Write", toolUse.Name)
	require.Equal(t, "hello.txt", toolUse.Input["file_path"])
}

func TestParseCodexFileChangeKindString(t *testing.T) {
	logger := slog.Default()

	data := map[string]any{
		"type": "item.completed",
		"item": map[string]any{
			"id":   "item-1",
			"type": "file_change",
			"changes": []any{
				map[string]any{
					"path": "hello.txt",
					"kind": "add",
				},
			},
		},
	}

	msg, err := Parse(logger, data)
	require.NoError(t, err, "current codex exec emits string file_change kinds like add")

	assistant, ok := msg.(*AssistantMessage)
	require.True(t, ok, "expected *AssistantMessage")
	require.Len(t, assistant.Content, 1)

	toolUse, ok := assistant.Content[0].(*ToolUseBlock)
	require.True(t, ok, "expected first content block to be ToolUseBlock")
	require.Equal(t, "Write", toolUse.Name, "string kind add should still surface as a file create")
	require.Equal(t, "hello.txt", toolUse.Input["file_path"])
}

func TestParseCodexMCPToolCall_CompletedPreservesResult(t *testing.T) {
	logger := slog.Default()

	msg, err := Parse(logger, map[string]any{
		"type": "item.completed",
		"item": map[string]any{
			"id":     "mcp_123",
			"type":   "mcp_tool_call",
			"server": "calc",
			"tool":   "add",
			"arguments": map[string]any{
				"a": 15.0,
				"b": 27.0,
			},
			"status": "completed",
			"result": map[string]any{
				"content": []any{
					map[string]any{
						"type": "text",
						"text": "42",
					},
				},
			},
			"contentItems": []any{
				map[string]any{
					"type": "text",
					"text": "42",
				},
			},
			"success": true,
		},
	})
	require.NoError(t, err)

	assistant, ok := msg.(*AssistantMessage)
	require.True(t, ok, "expected *AssistantMessage")
	require.Len(t, assistant.Content, 2, "completed MCP tool calls should preserve a result block")

	toolUse, ok := assistant.Content[0].(*ToolUseBlock)
	require.True(t, ok, "expected first content block to be ToolUseBlock")
	require.Equal(t, "calc:add", toolUse.Name)
	require.Equal(t, 15.0, toolUse.Input["a"])
	require.Equal(t, 27.0, toolUse.Input["b"])

	toolResult, ok := assistant.Content[1].(*ToolResultBlock)
	require.True(t, ok, "expected second content block to be ToolResultBlock")
	require.NotEmpty(t, toolResult.Content, "completed MCP tool call result payload should not be dropped")
}

func TestParseTypedSystemMessages(t *testing.T) {
	logger := slog.Default()

	t.Run("task.started", func(t *testing.T) {
		msg, err := Parse(logger, map[string]any{
			"type":    "system",
			"subtype": "task.started",
			"data": map[string]any{
				"turn_id":                 "turn_123",
				"collaboration_mode_kind": "plan",
				"model_context_window":    float64(256000),
			},
		})
		require.NoError(t, err)

		taskMsg, ok := msg.(*TaskStartedMessage)
		require.True(t, ok, "expected *TaskStartedMessage")
		require.Equal(t, "turn_123", taskMsg.TurnID)
		require.Equal(t, "plan", taskMsg.CollaborationModeKind)
		require.NotNil(t, taskMsg.ModelContextWindow)
		require.Equal(t, int64(256000), *taskMsg.ModelContextWindow)
	})

	t.Run("task.complete", func(t *testing.T) {
		msg, err := Parse(logger, map[string]any{
			"type":    "system",
			"subtype": "task.complete",
			"data": map[string]any{
				"turn_id":            "turn_456",
				"last_agent_message": "done",
			},
		})
		require.NoError(t, err)

		taskMsg, ok := msg.(*TaskCompleteMessage)
		require.True(t, ok, "expected *TaskCompleteMessage")
		require.Equal(t, "turn_456", taskMsg.TurnID)
		require.NotNil(t, taskMsg.LastAgentMessage)
		require.Equal(t, "done", *taskMsg.LastAgentMessage)
	})

	t.Run("thread.rolled_back", func(t *testing.T) {
		msg, err := Parse(logger, map[string]any{
			"type":    "system",
			"subtype": "thread.rolled_back",
			"data": map[string]any{
				"num_turns": float64(2),
			},
		})
		require.NoError(t, err)

		rollbackMsg, ok := msg.(*ThreadRolledBackMessage)
		require.True(t, ok, "expected *ThreadRolledBackMessage")
		require.Equal(t, 2, rollbackMsg.NumTurns)
	})

	t.Run("task.complete requires canonical data envelope", func(t *testing.T) {
		_, err := Parse(logger, map[string]any{
			"type":               "system",
			"subtype":            "task.complete",
			"turn_id":            "turn_456",
			"last_agent_message": "done",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), `"data"`)
	})
}

func TestParseCodexTodoList(t *testing.T) {
	logger := slog.Default()

	msg, err := Parse(logger, map[string]any{
		"type": "item.completed",
		"item": map[string]any{
			"id":   "todo_1",
			"type": "todo_list",
			"items": []any{
				map[string]any{"text": "Write tests", "completed": true},
				map[string]any{"text": "Deploy", "completed": false},
			},
		},
	})
	require.NoError(t, err)

	assistant, ok := msg.(*AssistantMessage)
	require.True(t, ok, "expected *AssistantMessage")
	require.Len(t, assistant.Content, 1)

	textBlock, ok := assistant.Content[0].(*TextBlock)
	require.True(t, ok, "expected TextBlock")
	require.Contains(t, textBlock.Text, "[x] Write tests")
	require.Contains(t, textBlock.Text, "[ ] Deploy")
}

func TestParseCodexToolSearch(t *testing.T) {
	logger := slog.Default()

	msg, err := Parse(logger, map[string]any{
		"type": "item.completed",
		"item": map[string]any{
			"id":   "ts_1",
			"type": "tool_search_call",
			"name": "SearchTools",
			"arguments": map[string]any{
				"query": "file search",
			},
			"success": true,
			"contentItems": []any{
				map[string]any{"type": "inputText", "text": "found 3 tools"},
			},
		},
	})
	require.NoError(t, err)

	assistant, ok := msg.(*AssistantMessage)
	require.True(t, ok, "expected *AssistantMessage")
	require.Len(t, assistant.Content, 2)

	toolUse, ok := assistant.Content[0].(*ToolUseBlock)
	require.True(t, ok, "expected ToolUseBlock")
	require.Equal(t, "SearchTools", toolUse.Name)
	require.Equal(t, "ts_1", toolUse.ID)
	require.Equal(t, "file search", toolUse.Input["query"])

	toolResult, ok := assistant.Content[1].(*ToolResultBlock)
	require.True(t, ok, "expected ToolResultBlock")
	require.Equal(t, "ts_1", toolResult.ToolUseID)
	require.False(t, toolResult.IsError)
}

func TestParseCodexToolSearch_DefaultName(t *testing.T) {
	logger := slog.Default()

	msg, err := Parse(logger, map[string]any{
		"type": "item.completed",
		"item": map[string]any{
			"id":   "ts_2",
			"type": "tool_search_call",
		},
	})
	require.NoError(t, err)

	assistant, ok := msg.(*AssistantMessage)
	require.True(t, ok)

	toolUse, ok := assistant.Content[0].(*ToolUseBlock)
	require.True(t, ok)
	require.Equal(t, "ToolSearch", toolUse.Name)
}

func TestParseCodexImageGeneration(t *testing.T) {
	logger := slog.Default()

	msg, err := Parse(logger, map[string]any{
		"type": "item.completed",
		"item": map[string]any{
			"id":      "ig_1",
			"type":    "image_generation_call",
			"name":    "GenerateImage",
			"success": true,
			"contentItems": []any{
				map[string]any{"type": "inputImage", "imageUrl": "data:image/png;base64,abc"},
			},
		},
	})
	require.NoError(t, err)

	assistant, ok := msg.(*AssistantMessage)
	require.True(t, ok)
	require.Len(t, assistant.Content, 2)

	toolUse, ok := assistant.Content[0].(*ToolUseBlock)
	require.True(t, ok)
	require.Equal(t, "GenerateImage", toolUse.Name)

	toolResult, ok := assistant.Content[1].(*ToolResultBlock)
	require.True(t, ok)
	require.Equal(t, "ig_1", toolResult.ToolUseID)
	require.False(t, toolResult.IsError)
}

func TestParseCodexCustomToolCall(t *testing.T) {
	logger := slog.Default()

	succTrue := true

	msg, err := Parse(logger, map[string]any{
		"type": "item.completed",
		"item": map[string]any{
			"id":   "ct_1",
			"type": "custom_tool_call",
			"name": "my_tool",
			"arguments": map[string]any{
				"input": "test",
			},
			"success": succTrue,
			"contentItems": []any{
				map[string]any{"type": "inputText", "text": "result"},
			},
		},
	})
	require.NoError(t, err)

	assistant, ok := msg.(*AssistantMessage)
	require.True(t, ok)
	require.Len(t, assistant.Content, 2)

	toolUse, ok := assistant.Content[0].(*ToolUseBlock)
	require.True(t, ok)
	require.Equal(t, "my_tool", toolUse.Name)

	toolResult, ok := assistant.Content[1].(*ToolResultBlock)
	require.True(t, ok)
	require.Equal(t, "ct_1", toolResult.ToolUseID)
	require.False(t, toolResult.IsError)
	require.Len(t, toolResult.Content, 1)
}

func TestParseContentBlock_Image(t *testing.T) {
	block, err := parseContentBlock(map[string]any{
		"type": "image",
		"url":  "https://example.com/img.png",
	})
	require.NoError(t, err)

	imgBlock, ok := block.(*InputImageBlock)
	require.True(t, ok)
	require.Equal(t, "https://example.com/img.png", imgBlock.URL)
}

func TestParseContentBlock_ImageWithImageURL(t *testing.T) {
	block, err := parseContentBlock(map[string]any{
		"type":      "image",
		"image_url": "data:image/png;base64,abc",
	})
	require.NoError(t, err)

	imgBlock, ok := block.(*InputImageBlock)
	require.True(t, ok)
	require.Equal(t, "data:image/png;base64,abc", imgBlock.URL)
}

func TestParseContentBlock_LocalImage(t *testing.T) {
	block, err := parseContentBlock(map[string]any{
		"type": "local_image",
		"path": "/tmp/screenshot.png",
	})
	require.NoError(t, err)

	localBlock, ok := block.(*InputLocalImageBlock)
	require.True(t, ok)
	require.Equal(t, BlockTypeLocalImage, localBlock.Type)
	require.Equal(t, "/tmp/screenshot.png", localBlock.Path)
}

func TestParseContentBlock_LocalImageCamelCase(t *testing.T) {
	block, err := parseContentBlock(map[string]any{
		"type": "localImage",
		"path": "/tmp/img.jpg",
	})
	require.NoError(t, err)

	localBlock, ok := block.(*InputLocalImageBlock)
	require.True(t, ok)
	require.Equal(t, "/tmp/img.jpg", localBlock.Path)
}

func TestParseContentBlock_Mention(t *testing.T) {
	block, err := parseContentBlock(map[string]any{
		"type": "mention",
		"name": "src/main.go",
		"path": "/home/user/project/src/main.go",
	})
	require.NoError(t, err)

	mentionBlock, ok := block.(*InputMentionBlock)
	require.True(t, ok)
	require.Equal(t, "src/main.go", mentionBlock.Name)
	require.Equal(t, "/home/user/project/src/main.go", mentionBlock.Path)
}

func TestContentItemsToBlocks_InputImage(t *testing.T) {
	items := []ContentItem{
		{Type: "inputText", Text: "hello"},
		{
			Type: "inputImage",
			Raw:  map[string]any{"type": "inputImage", "imageUrl": "https://example.com/img.png"},
		},
		{
			Type: "inputImage",
			Raw:  map[string]any{"type": "inputImage", "image_url": "data:image/png;base64,abc"},
		},
	}

	blocks := contentItemsToBlocks(items)
	require.Len(t, blocks, 3)

	textBlock, ok := blocks[0].(*TextBlock)
	require.True(t, ok)
	require.Equal(t, "hello", textBlock.Text)

	imgBlock1, ok := blocks[1].(*InputImageBlock)
	require.True(t, ok)
	require.Equal(t, "https://example.com/img.png", imgBlock1.URL)

	imgBlock2, ok := blocks[2].(*InputImageBlock)
	require.True(t, ok)
	require.Equal(t, "data:image/png;base64,abc", imgBlock2.URL)
}

func TestCodexUsage_ReasoningOutputTokens(t *testing.T) {
	logger := slog.Default()

	msg, err := Parse(logger, map[string]any{
		"type":       "turn.completed",
		"session_id": "sess_1",
		"usage": map[string]any{
			"input_tokens":            float64(100),
			"cached_input_tokens":     float64(50),
			"output_tokens":           float64(200),
			"reasoning_output_tokens": float64(75),
		},
	})
	require.NoError(t, err)

	result, ok := msg.(*ResultMessage)
	require.True(t, ok)
	require.NotNil(t, result.Usage)
	require.Equal(t, 75, result.Usage.ReasoningOutputTokens)
}

func TestParse_AttachesAuditEnvelope_AssistantMessage(t *testing.T) {
	logger := slog.Default()

	data := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": "hello"},
			},
			"model": "codex-mini-latest",
		},
	}

	msg, err := Parse(logger, data)
	require.NoError(t, err)

	am, ok := msg.(*AssistantMessage)
	require.True(t, ok)
	require.NotNil(t, am.Audit)
	assert.Equal(t, "assistant", am.Audit.EventType)
	assert.NotNil(t, am.Audit.Payload)

	var payload map[string]any

	err = json.Unmarshal(am.Audit.Payload, &payload)
	require.NoError(t, err)
	assert.Equal(t, "assistant", payload["type"])
}

func TestParse_AttachesAuditEnvelope_SystemMessage(t *testing.T) {
	logger := slog.Default()

	data := map[string]any{
		"type":    "system",
		"subtype": "task.started",
		"data":    map[string]any{"turn_id": "turn-1"},
	}

	msg, err := Parse(logger, data)
	require.NoError(t, err)

	sm, ok := msg.(*TaskStartedMessage)
	require.True(t, ok)
	require.NotNil(t, sm.Audit)
	assert.Equal(t, "system", sm.Audit.EventType)
	assert.Equal(t, "task.started", sm.Audit.Subtype)
}

func TestParse_AttachesAuditEnvelope_ResultMessage(t *testing.T) {
	logger := slog.Default()

	data := map[string]any{
		"type":       "result",
		"subtype":    "success",
		"is_error":   false,
		"session_id": "sess-1",
		"result":     "done",
	}

	msg, err := Parse(logger, data)
	require.NoError(t, err)

	rm, ok := msg.(*ResultMessage)
	require.True(t, ok)
	require.NotNil(t, rm.Audit)
	assert.Equal(t, "result", rm.Audit.EventType)
	assert.Equal(t, "success", rm.Audit.Subtype)
}

func TestParse_AuditPayloadPreservesRawWireData(t *testing.T) {
	logger := slog.Default()

	data := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{},
			"model":   "test-model",
		},
		"custom_field": "preserved",
	}

	msg, err := Parse(logger, data)
	require.NoError(t, err)

	am, ok := msg.(*AssistantMessage)
	require.True(t, ok)
	require.NotNil(t, am.Audit)

	var payload map[string]any

	err = json.Unmarshal(am.Audit.Payload, &payload)
	require.NoError(t, err)
	assert.Equal(t, "preserved", payload["custom_field"])
}

func TestParse_AuditPayloadPreservesOriginalJSONBytes(t *testing.T) {
	logger := slog.Default()
	raw := []byte("{\n  \"type\": \"assistant\",\n  \"message\": {\n    \"content\": [],\n    \"model\": \"test-model\"\n  },\n  \"provider_trace\": 1e+06,\n  \"provider_trace\": 1000000\n}")

	msg, err := Parse(logger, raw)
	require.NoError(t, err)

	am, ok := msg.(*AssistantMessage)
	require.True(t, ok)
	require.NotNil(t, am.Audit)
	assert.Equal(t, string(raw), string(am.Audit.Payload))
}

func TestNewAuditEnvelope_PublicConstructor(t *testing.T) {
	type testPayload struct {
		Key string `json:"key"`
	}

	env, err := NewAuditEnvelope("test_event", "test_sub", testPayload{Key: "val"})
	require.NoError(t, err)
	require.NotNil(t, env)
	assert.Equal(t, "test_event", env.EventType)
	assert.Equal(t, "test_sub", env.Subtype)

	var payload map[string]any

	err = json.Unmarshal(env.Payload, &payload)
	require.NoError(t, err)
	assert.Equal(t, "val", payload["key"])
}

func TestNewAuditEnvelope_MarshalError(t *testing.T) {
	env, err := NewAuditEnvelope("event", "sub", make(chan int))
	assert.Error(t, err)
	assert.Nil(t, env)
}

func TestParseResultMessage_DirectFields(t *testing.T) {
	t.Parallel()

	logger := slog.Default()
	stopReason := "end_turn"
	cost := 0.0042

	data := map[string]any{
		"type":           "result",
		"subtype":        "success",
		"is_error":       false,
		"session_id":     "sess-123",
		"stop_reason":    stopReason,
		"duration_ms":    float64(1500),
		"num_turns":      float64(3),
		"total_cost_usd": cost,
		"result":         "hello",
		"usage": map[string]any{
			"input_tokens":  float64(100),
			"output_tokens": float64(50),
		},
	}

	msg, err := Parse(logger, data)
	require.NoError(t, err)

	rm, ok := msg.(*ResultMessage)
	require.True(t, ok)

	assert.Equal(t, "sess-123", rm.SessionID)
	require.NotNil(t, rm.StopReason)
	assert.Equal(t, "end_turn", *rm.StopReason)
	assert.Equal(t, 1500, rm.DurationMs)
	assert.Equal(t, 3, rm.NumTurns)
	require.NotNil(t, rm.TotalCostUSD)
	assert.InDelta(t, cost, *rm.TotalCostUSD, 1e-9)
}

func TestParseCodexTurnCompleted_DirectFields(t *testing.T) {
	t.Parallel()

	cost := 0.125

	data := map[string]any{
		"type":           "response.completed",
		"stop_reason":    "end_turn",
		"duration_ms":    float64(2500),
		"num_turns":      float64(7),
		"total_cost_usd": cost,
		"usage": map[string]any{
			"input_tokens":            float64(200),
			"output_tokens":           float64(100),
			"cached_input_tokens":     float64(50),
			"reasoning_output_tokens": float64(25),
		},
	}

	msg, err := parseCodexTurnCompleted(data)
	require.NoError(t, err)

	require.NotNil(t, msg.StopReason)
	assert.Equal(t, "end_turn", *msg.StopReason)
	assert.Equal(t, 2500, msg.DurationMs)
	assert.Equal(t, 7, msg.NumTurns)
	require.NotNil(t, msg.TotalCostUSD)
	assert.InDelta(t, cost, *msg.TotalCostUSD, 1e-9)
}

func TestParseCodexTurnCompleted_MissingOptionalFields(t *testing.T) {
	t.Parallel()

	data := map[string]any{
		"type": "response.completed",
	}

	msg, err := parseCodexTurnCompleted(data)
	require.NoError(t, err)

	assert.Nil(t, msg.StopReason)
	assert.Equal(t, 0, msg.DurationMs)
	assert.Equal(t, 0, msg.NumTurns)
	assert.Nil(t, msg.TotalCostUSD)
}
