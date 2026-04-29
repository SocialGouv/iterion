package tools

import (
	"context"

	intl "github.com/SocialGouv/claw-code-go/internal/tools"
	"github.com/SocialGouv/claw-code-go/pkg/api"
)

// PlanModeState is the mutable flag + on-disk state directory shared
// by enter_plan_mode and exit_plan_mode so they can coordinate.
type PlanModeState struct {
	Active *bool
	Dir    string
}

func EnterPlanModeTool() api.Tool { return intl.EnterPlanModeTool() }

func ExecuteEnterPlanMode(ctx context.Context, input map[string]any, state *PlanModeState) (string, error) {
	if state == nil {
		return intl.ExecuteEnterPlanMode(nil, "")
	}
	return intl.ExecuteEnterPlanMode(state.Active, state.Dir)
}

func ExitPlanModeTool() api.Tool { return intl.ExitPlanModeTool() }

func ExecuteExitPlanMode(ctx context.Context, input map[string]any, state *PlanModeState) (string, error) {
	if state == nil {
		return intl.ExecuteExitPlanMode(nil, "")
	}
	return intl.ExecuteExitPlanMode(state.Active, state.Dir)
}
