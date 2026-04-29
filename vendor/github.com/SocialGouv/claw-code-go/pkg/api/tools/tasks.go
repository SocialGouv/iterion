package tools

import (
	"context"

	intl "github.com/SocialGouv/claw-code-go/internal/tools"
	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/claw-code-go/pkg/api/task"
)

// All task_* tools share the same *task.Registry. Build it once via
// task.NewRegistry() and pass the same instance to every Execute.

func TaskCreateTool() api.Tool { return intl.TaskCreateTool() }

func ExecuteTaskCreate(ctx context.Context, input map[string]any, reg *task.Registry) (string, error) {
	return intl.ExecuteTaskCreate(input, reg)
}

func TaskGetTool() api.Tool { return intl.TaskGetTool() }

func ExecuteTaskGet(ctx context.Context, input map[string]any, reg *task.Registry) (string, error) {
	return intl.ExecuteTaskGet(input, reg)
}

func TaskListTool() api.Tool { return intl.TaskListTool() }

func ExecuteTaskList(ctx context.Context, input map[string]any, reg *task.Registry) (string, error) {
	return intl.ExecuteTaskList(input, reg)
}

func TaskOutputTool() api.Tool { return intl.TaskOutputTool() }

func ExecuteTaskOutput(ctx context.Context, input map[string]any, reg *task.Registry) (string, error) {
	return intl.ExecuteTaskOutput(input, reg)
}

func TaskStopTool() api.Tool { return intl.TaskStopTool() }

func ExecuteTaskStop(ctx context.Context, input map[string]any, reg *task.Registry) (string, error) {
	return intl.ExecuteTaskStop(input, reg)
}

func TaskUpdateTool() api.Tool { return intl.TaskUpdateTool() }

func ExecuteTaskUpdate(ctx context.Context, input map[string]any, reg *task.Registry) (string, error) {
	return intl.ExecuteTaskUpdate(input, reg)
}

func RunTaskPacketTool() api.Tool { return intl.RunTaskPacketTool() }

func ExecuteRunTaskPacket(ctx context.Context, input map[string]any, reg *task.Registry) (string, error) {
	return intl.ExecuteRunTaskPacket(input, reg)
}
