package tools

import (
	"context"

	intl "github.com/SocialGouv/claw-code-go/internal/tools"
	"github.com/SocialGouv/claw-code-go/pkg/api"
)

func SkillTool() api.Tool { return intl.SkillTool() }

// ExecuteSkill loads a skill from <workDir>/.claude/skills. Pass an
// empty workDir to use the process CWD.
func ExecuteSkill(ctx context.Context, input map[string]any, workDir string) (string, error) {
	return intl.ExecuteSkill(input, workDir)
}
