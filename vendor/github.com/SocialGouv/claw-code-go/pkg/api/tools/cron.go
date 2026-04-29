package tools

import (
	"context"

	intl "github.com/SocialGouv/claw-code-go/internal/tools"
	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/claw-code-go/pkg/api/team"
)

// All cron_* tools share a *team.CronRegistry.

func CronCreateTool() api.Tool { return intl.CronCreateTool() }

func ExecuteCronCreate(ctx context.Context, input map[string]any, reg *team.CronRegistry) (string, error) {
	return intl.ExecuteCronCreate(input, reg)
}

func CronGetTool() api.Tool { return intl.CronGetTool() }

func ExecuteCronGet(ctx context.Context, input map[string]any, reg *team.CronRegistry) (string, error) {
	return intl.ExecuteCronGet(input, reg)
}

func CronListTool() api.Tool { return intl.CronListTool() }

func ExecuteCronList(ctx context.Context, input map[string]any, reg *team.CronRegistry) (string, error) {
	return intl.ExecuteCronList(input, reg)
}

func CronDeleteTool() api.Tool { return intl.CronDeleteTool() }

func ExecuteCronDelete(ctx context.Context, input map[string]any, reg *team.CronRegistry) (string, error) {
	return intl.ExecuteCronDelete(input, reg)
}
