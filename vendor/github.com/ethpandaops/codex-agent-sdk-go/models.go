package codexsdk

import (
	"context"
	"fmt"
)

// ListModels spawns a temporary Codex CLI session to discover available models.
// The session is automatically closed after all model-list pages are retrieved.
func ListModels(ctx context.Context, opts ...Option) ([]ModelInfo, error) {
	resp, err := ListModelsResponse(ctx, opts...)
	if err != nil {
		return nil, err
	}

	return resp.Models, nil
}

// ListModelsResponse spawns a temporary Codex CLI session to discover available models.
// The session is automatically closed after all model-list pages are retrieved.
func ListModelsResponse(ctx context.Context, opts ...Option) (*ModelListResponse, error) {
	var resp *ModelListResponse

	err := WithClient(ctx, func(c Client) error {
		var err error

		resp, err = c.ListModelsResponse(ctx)
		if err != nil {
			return fmt.Errorf("list models response: %w", err)
		}

		return nil
	}, opts...)
	if err != nil {
		return nil, err
	}

	return resp, nil
}
