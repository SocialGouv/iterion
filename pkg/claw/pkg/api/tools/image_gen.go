package tools

import (
	"context"

	intl "github.com/SocialGouv/iterion/pkg/claw/internal/tools"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
)

func ImageGenTool() api.Tool { return intl.ImageGenTool() }

func ExecuteImageGen(ctx context.Context, input map[string]any, generator api.ImageGenerator, outputDir string) (string, error) {
	return intl.ExecuteImageGen(ctx, input, generator, outputDir)
}
