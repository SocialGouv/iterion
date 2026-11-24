package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// F3: the router hashes the digest over the integration's bots, the set the
// projector hashed — never over the webhook config's bot scope, which an
// operator may PATCH apart from the integration. A drifted config scope
// still routes; a drifted integration (bots the rows were not projected for)
// is stale until a sync.
//
// Mutation: spineDigestStale hashes cfg.BotIDs ∪ the rows' bots on an
// integration binding → red on "a PATCHed config scope".
func TestSpineIssueLabel_DigestHashesTheIntegrationsBots(t *testing.T) {
	t.Run("config scope patched apart from the integration", func(t *testing.T) {
		f := newSpineGitHubFixture(t,
			spineBotDecl{bot: "nestor", mode: bundle.ExecutionDirect, labels: []string{"nestor"}})
		f.cfg.BotIDs = append(f.cfg.BotIDs, defaultWebhookBotReviewPR)
		w := f.deliver(f.body("nestor"))
		if w.Code != http.StatusAccepted {
			t.Fatalf("a PATCHed config scope: code=%d body=%s, want 202 — the rows are the integration's projection, still current", w.Code, w.Body.String())
		}
	})
	t.Run("integration bots the rows were not projected for", func(t *testing.T) {
		f := newSpineGitHubFixture(t,
			spineBotDecl{bot: "nestor", mode: bundle.ExecutionDirect, labels: []string{"nestor"}})
		ri, err := f.s.forgeIntegrations.Get(context.Background(), spineIntegrationID)
		if err != nil {
			t.Fatal(err)
		}
		ri.BotIDs = append(ri.BotIDs, defaultWebhookBotReviewPR)
		spineStoreIntegration(t, f.s, ri)
		w := f.deliver(f.body("nestor"))
		if w.Code != http.StatusServiceUnavailable || laneRespAny(t, w)["status"] != "subscriptions_stale" {
			t.Fatalf("an integration whose bots moved without a sync: code=%d body=%s, want 503 subscriptions_stale", w.Code, w.Body.String())
		}
	})
}
