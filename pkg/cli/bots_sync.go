package cli

import (
	"context"
	"strings"

	"github.com/SocialGouv/iterion/pkg/botdeps"
)

// BotSyncResult describes one materialized lockfile dependency.
type BotSyncResult = botdeps.SyncResult

// BotsSync materializes every project-root bots.lock dependency into .botz.
// An error comes back with the dependencies materialized before it, whose
// CatalogError the caller still owes its user.
func BotsSync(ctx context.Context, workdir string) ([]BotSyncResult, error) {
	return botdeps.Sync(ctx, workdir)
}

// DistinctCatalogErrors returns the catalog refresh failures a sync reported,
// once each, in the order the dependencies were materialized. A workspace
// holds one catalog, so N dependencies installed against a broken owner
// report the same cause N times — the operator reads it once, under the rows
// it applies to.
func DistinctCatalogErrors(results []BotSyncResult) []string {
	seen := make(map[string]struct{}, len(results))
	causes := make([]string, 0, 1)
	for _, result := range results {
		cause := strings.TrimSpace(result.CatalogError)
		if cause == "" {
			continue
		}
		if _, dup := seen[cause]; dup {
			continue
		}
		seen[cause] = struct{}{}
		causes = append(causes, cause)
	}
	return causes
}
