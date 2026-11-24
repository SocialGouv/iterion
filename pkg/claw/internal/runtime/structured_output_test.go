package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
	"github.com/SocialGouv/iterion/pkg/claw/internal/tools"
)

func structuredOutputInput() map[string]any {
	return map[string]any{"payload": map[string]any{"status": "complete"}}
}

func TestStructuredOutputRequiresCompletedWork(t *testing.T) {
	loop := NewConversationLoop(&Config{Model: "test"}, nil)

	first := loop.ExecuteTool(context.Background(), "structured_output", structuredOutputInput())
	if !first.IsError {
		t.Fatalf("first structured_output call should be rejected: %+v", first)
	}
	if len(first.Content) == 0 || !strings.Contains(first.Content[0].Text, structuredOutputRequiresWorkMessage) {
		t.Fatalf("rejection does not explain how to proceed: %+v", first.Content)
	}

	work := loop.ExecuteTool(context.Background(), "read_file", map[string]any{"path": "structured_output.go"})
	if work.IsError {
		t.Fatalf("work tool should succeed: %+v", work.Content)
	}

	final := loop.ExecuteTool(context.Background(), "structured_output", structuredOutputInput())
	if final.IsError {
		t.Fatalf("structured_output should succeed after completed work: %+v", final.Content)
	}
	if len(final.Content) == 0 || !strings.Contains(final.Content[0].Text, "Structured output provided successfully") {
		t.Fatalf("unexpected structured_output result: %+v", final.Content)
	}
}

func TestStructuredOutputRequiresCompletedWorkThroughQuietDispatcher(t *testing.T) {
	loop := NewConversationLoop(&Config{Model: "test"}, nil)

	first := loop.ExecuteToolQuiet(context.Background(), "structured_output", structuredOutputInput())
	if !first.IsError || len(first.Content) == 0 || !strings.Contains(first.Content[0].Text, structuredOutputRequiresWorkMessage) {
		t.Fatalf("quiet dispatcher should apply the shared guard: %+v", first)
	}

	work := loop.ExecuteToolQuiet(context.Background(), "read_file", map[string]any{"path": "structured_output.go"})
	if work.IsError {
		t.Fatalf("quiet work tool should succeed: %+v", work.Content)
	}
	if final := loop.ExecuteToolQuiet(context.Background(), "structured_output", structuredOutputInput()); final.IsError {
		t.Fatalf("quiet structured_output should succeed after work: %+v", final.Content)
	}
}

func TestStructuredOutputAllowsImmediateWithoutWorkTools(t *testing.T) {
	tests := []struct {
		name  string
		tools []api.Tool
	}{
		{name: "empty tool list"},
		{
			name: "only excluded tools",
			tools: []api.Tool{
				tools.StructuredOutputTool(),
				tools.AskUserQuestionTool(),
				tools.SleepTool(),
				tools.ConfigTool(),
				tools.TodoWriteTool(),
				tools.EnterPlanModeTool(),
				tools.ExitPlanModeTool(),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loop := NewConversationLoop(&Config{Model: "test"}, nil)
			loop.Tools = tt.tools

			result := loop.ExecuteTool(context.Background(), "structured_output", structuredOutputInput())
			if result.IsError {
				t.Fatalf("session without work tools should allow immediate structured_output: %+v", result.Content)
			}
		})
	}
}

func TestStructuredOutputAllowsImmediateWhenConfigured(t *testing.T) {
	loop := NewConversationLoop(&Config{
		Model:                          "test",
		AllowImmediateStructuredOutput: true,
	}, nil)

	result := loop.ExecuteTool(context.Background(), "structured_output", structuredOutputInput())
	if result.IsError {
		t.Fatalf("escape hatch should allow immediate structured_output: %+v", result.Content)
	}
}
