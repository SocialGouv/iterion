package tools

import (
	"context"

	intl "github.com/SocialGouv/claw-code-go/internal/tools"
	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/claw-code-go/pkg/api/lsp"
)

func LspTool() api.Tool { return intl.LspTool() }

func ExecuteLSP(ctx context.Context, input map[string]any, registry *lsp.Registry) (string, error) {
	return intl.ExecuteLSP(input, registry)
}
