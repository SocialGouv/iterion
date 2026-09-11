package cli

import (
	"context"

	"github.com/SocialGouv/iterion/pkg/botdeps"
)

// BotSyncResult describes one materialized lockfile dependency.
type BotSyncResult = botdeps.SyncResult

// BotsSync materializes every project-root bots.lock dependency into .botz.
func BotsSync(ctx context.Context, workdir string) ([]BotSyncResult, error) {
	return botdeps.Sync(ctx, workdir)
}
