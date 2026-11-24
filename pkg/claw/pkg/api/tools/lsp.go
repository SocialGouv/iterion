package tools

import (
	"context"

	intl "github.com/SocialGouv/iterion/pkg/claw/internal/tools"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api/lsp"
)

func LspTool() api.Tool { return intl.LspTool() }

func ExecuteLSP(ctx context.Context, input map[string]any, registry *lsp.Registry) (string, error) {
	return intl.ExecuteLSP(input, registry)
}
