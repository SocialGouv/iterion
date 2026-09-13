package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/runops"
	"github.com/SocialGouv/iterion/pkg/store"
)

type RunConfig struct {
	Store        store.RunStore
	Capabilities []string
}

// RegisterClawRunTools exposes the shared runops surface in-process. Store is
// already project-scoped locally and tenant-scoped in cloud, so no model or
// tool argument can select another store.
func RegisterClawRunTools(reg *Registry, cfg *RunConfig) error {
	if cfg == nil || cfg.Store == nil {
		return nil
	}
	caps := runops.NewCapabilities(cfg.Capabilities...)
	for _, definition := range runops.ToolsFor(caps) {
		definition := definition
		exec := func(ctx context.Context, input json.RawMessage) (string, error) {
			raw, err := runops.Call(ctx, cfg.Store, caps, definition.Name, input)
			if err != nil {
				return "", err
			}
			return string(raw), nil
		}
		if err := reg.RegisterMCP("iterion_runs", definition.Name, definition.Description, definition.InputSchema, exec); err != nil {
			return fmt.Errorf("register iterion_runs/%s: %w", definition.Name, err)
		}
	}
	return nil
}
