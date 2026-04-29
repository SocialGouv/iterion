package tools

import (
	"context"

	intl "github.com/SocialGouv/claw-code-go/internal/tools"
	"github.com/SocialGouv/claw-code-go/pkg/api"
)

func TodoWriteTool() api.Tool { return intl.TodoWriteTool() }

func ExecuteTodoWrite(ctx context.Context, input map[string]any) (string, error) {
	return intl.ExecuteTodoWrite(input)
}

func WebSearchTool() api.Tool { return intl.WebSearchTool() }

// ExecuteWebSearch reads BRAVE_API_KEY (or compatible) from env;
// absence surfaces as a tool error to the model.
func ExecuteWebSearch(ctx context.Context, input map[string]any) (string, error) {
	return intl.ExecuteWebSearch(input)
}

func SendUserMessageTool() api.Tool { return intl.SendUserMessageTool() }

func ExecuteSendUserMessage(ctx context.Context, input map[string]any) (string, error) {
	return intl.ExecuteSendUserMessage(input)
}

func RemoteTriggerTool() api.Tool { return intl.RemoteTriggerTool() }

func ExecuteRemoteTrigger(ctx context.Context, input map[string]any) (string, error) {
	return intl.ExecuteRemoteTrigger(input)
}

func SleepTool() api.Tool { return intl.SleepTool() }

func ExecuteSleep(ctx context.Context, input map[string]any) (string, error) {
	return intl.ExecuteSleep(input)
}

func NotebookEditTool() api.Tool { return intl.NotebookEditTool() }

func ExecuteNotebookEdit(ctx context.Context, input map[string]any) (string, error) {
	return intl.ExecuteNotebookEdit(input)
}

func REPLTool() api.Tool { return intl.REPLTool() }

func ExecuteREPL(ctx context.Context, input map[string]any) (string, error) {
	return intl.ExecuteREPL(input)
}

// AgentTool returns the subagent-spawn tool. The internal executor
// only validates and returns metadata; orchestration is the host's
// job (iterion's claw backend already routes tool_use back).
func AgentTool() api.Tool { return intl.AgentTool() }

func ExecuteAgent(ctx context.Context, input map[string]any) (string, error) {
	return intl.ExecuteAgent(input)
}

func StructuredOutputTool() api.Tool { return intl.StructuredOutputTool() }

func ExecuteStructuredOutput(ctx context.Context, input map[string]any) (string, error) {
	return intl.ExecuteStructuredOutput(input)
}
