package tools

import (
	"context"

	intl "github.com/SocialGouv/claw-code-go/internal/tools"
	"github.com/SocialGouv/claw-code-go/pkg/api"
)

func ImageGenTool() api.Tool { return intl.ImageGenTool() }

func ExecuteImageGen(ctx context.Context, input map[string]any, generator api.ImageGenerator, outputDir string) (string, error) {
	return intl.ExecuteImageGen(ctx, input, generator, outputDir)
}
