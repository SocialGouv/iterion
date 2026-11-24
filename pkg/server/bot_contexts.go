package server

import (
	"context"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/pluginsource"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// botContextResolver builds the launch-time materialiser of operator-bound
// context documents (ADR-147, #1908) from the server's binding store and the
// SHARED pinned-source fetcher — the same pluginsource.Fetcher the plugin
// contribution resolver uses, so one cache, one pin doctrine and one
// credential-by-reference handling serve both. Nil when either ingredient is
// missing: the launch paths then behave as before the seam existed, and the
// binding route keeps refusing the context kind — a binding accepted on a
// server that cannot materialise it would be an operator-visible no-op,
// which is the very defect the kind exists to fix.
func (s *Server) botContextResolver() *runview.BotContextResolver {
	if s.botBindings == nil || s.pluginSourceFetcher == nil {
		return nil
	}
	fetcher := s.pluginSourceFetcher
	return &runview.BotContextResolver{
		Bindings: s.botBindings,
		Fetch: func(ctx context.Context, b secrets.BotSecretBinding) (string, error) {
			src := b.Context
			if src == nil {
				return "", fmt.Errorf("a context binding without its pinned source")
			}
			// The fetcher's cache key prefixes with the source name for
			// readability; uniqueness comes from the (url, ref) hash, so
			// the binding's id — filesystem-safe by construction — is
			// enough of a label.
			return fetcher.Fetch(ctx, pluginsource.PluginSource{
				TenantID: b.TenantID,
				Name:     "botctx-" + b.ID,
				GitURL:   src.GitURL,
				Ref:      src.Ref,
				SecretID: src.SecretID,
			})
		},
	}
}
