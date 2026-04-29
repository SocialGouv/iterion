package tools

import (
	"context"

	intl "github.com/SocialGouv/claw-code-go/internal/tools"
	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/claw-code-go/pkg/api/worker"
)

// All worker_* tools share a single *worker.WorkerRegistry that
// tracks subprocess lifecycle.

func WorkerCreateTool() api.Tool { return intl.WorkerCreateTool() }

func ExecuteWorkerCreate(ctx context.Context, input map[string]any, reg *worker.WorkerRegistry) (string, error) {
	return intl.ExecuteWorkerCreate(input, reg)
}

func WorkerGetTool() api.Tool { return intl.WorkerGetTool() }

func ExecuteWorkerGet(ctx context.Context, input map[string]any, reg *worker.WorkerRegistry) (string, error) {
	return intl.ExecuteWorkerGet(input, reg)
}

func WorkerObserveTool() api.Tool { return intl.WorkerObserveTool() }

func ExecuteWorkerObserve(ctx context.Context, input map[string]any, reg *worker.WorkerRegistry) (string, error) {
	return intl.ExecuteWorkerObserve(input, reg)
}

func WorkerResolveTrustTool() api.Tool { return intl.WorkerResolveTrustTool() }

func ExecuteWorkerResolveTrust(ctx context.Context, input map[string]any, reg *worker.WorkerRegistry) (string, error) {
	return intl.ExecuteWorkerResolveTrust(input, reg)
}

func WorkerAwaitReadyTool() api.Tool { return intl.WorkerAwaitReadyTool() }

func ExecuteWorkerAwaitReady(ctx context.Context, input map[string]any, reg *worker.WorkerRegistry) (string, error) {
	return intl.ExecuteWorkerAwaitReady(input, reg)
}

func WorkerSendPromptTool() api.Tool { return intl.WorkerSendPromptTool() }

func ExecuteWorkerSendPrompt(ctx context.Context, input map[string]any, reg *worker.WorkerRegistry) (string, error) {
	return intl.ExecuteWorkerSendPrompt(input, reg)
}

func WorkerRestartTool() api.Tool { return intl.WorkerRestartTool() }

func ExecuteWorkerRestart(ctx context.Context, input map[string]any, reg *worker.WorkerRegistry) (string, error) {
	return intl.ExecuteWorkerRestart(input, reg)
}

func WorkerTerminateTool() api.Tool { return intl.WorkerTerminateTool() }

func ExecuteWorkerTerminate(ctx context.Context, input map[string]any, reg *worker.WorkerRegistry) (string, error) {
	return intl.ExecuteWorkerTerminate(input, reg)
}

func WorkerObserveCompletionTool() api.Tool { return intl.WorkerObserveCompletionTool() }

func ExecuteWorkerObserveCompletion(ctx context.Context, input map[string]any, reg *worker.WorkerRegistry) (string, error) {
	return intl.ExecuteWorkerObserveCompletion(input, reg)
}
