package cli

import (
	"context"

	"github.com/SocialGouv/iterion/pkg/botdeps"
)

type BotsUpdateOptions = botdeps.UpdateOptions
type BotsUpdateResult = botdeps.UpdateResult

// BotsUpdate resolves one existing dependency, rewrites its pin atomically,
// and materializes exactly that dependency. It remains the CLI facade around
// the shared implementation used by the Studio host action.
func BotsUpdate(ctx context.Context, opts BotsUpdateOptions) (*BotsUpdateResult, error) {
	return botdeps.Update(ctx, opts)
}
