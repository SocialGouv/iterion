package tools

import (
	"context"

	intl "github.com/SocialGouv/claw-code-go/internal/tools"
	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/claw-code-go/pkg/api/team"
)

// All team_* tools share a *team.TeamRegistry.

func TeamCreateTool() api.Tool { return intl.TeamCreateTool() }

func ExecuteTeamCreate(ctx context.Context, input map[string]any, reg *team.TeamRegistry) (string, error) {
	return intl.ExecuteTeamCreate(input, reg)
}

func TeamGetTool() api.Tool { return intl.TeamGetTool() }

func ExecuteTeamGet(ctx context.Context, input map[string]any, reg *team.TeamRegistry) (string, error) {
	return intl.ExecuteTeamGet(input, reg)
}

func TeamListTool() api.Tool { return intl.TeamListTool() }

func ExecuteTeamList(ctx context.Context, input map[string]any, reg *team.TeamRegistry) (string, error) {
	return intl.ExecuteTeamList(input, reg)
}

func TeamDeleteTool() api.Tool { return intl.TeamDeleteTool() }

func ExecuteTeamDelete(ctx context.Context, input map[string]any, reg *team.TeamRegistry) (string, error) {
	return intl.ExecuteTeamDelete(input, reg)
}
