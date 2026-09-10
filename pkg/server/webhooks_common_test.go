package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// The bot-author actor guard and the re-request-review trigger are the two
// halves of ONE loop guard, and must read ONE identity set (iterionBotLogins).
// An identity the trigger recognises but the actor check doesn't lets the
// bot's own reviewer-write echo launch a review of itself; the converse
// treats a human account's review requests as bot triggers. This table pins
// the symmetry for every provider × connection shape.
func TestIterionBotIdentity_OneSetForBothGuards(t *testing.T) {
	cases := []struct {
		name     string
		provider webhooks.Provider
		conn     forge.Connection
		login    string
		isBot    bool
	}{
		{"gitlab account is the bot", webhooks.ProviderGitLab, forge.Connection{AccountLogin: "iterion-bot"}, "iterion-bot", true},
		{"gitlab other login", webhooks.ProviderGitLab, forge.Connection{AccountLogin: "iterion-bot"}, "alice", false},
		// A GitHub/Forgejo PAT/OAuth connection may be a HUMAN's personal
		// account — its login must NEVER read as iterion's identity: as an
		// author it would make that human's PRs unreviewable, as a reviewer
		// target it would turn an ordinary human-to-human review request
		// into an LLM launch (hold-label exempt, repeatable).
		{"github account login is NOT the bot", webhooks.ProviderGitHub, forge.Connection{AccountLogin: "alice-maintainer"}, "alice-maintainer", false},
		{"forgejo account login is NOT the bot", webhooks.ProviderForgejo, forge.Connection{AccountLogin: "alice-maintainer"}, "alice-maintainer", false},
		{"github app slug is the bot", webhooks.ProviderGitHub, forge.Connection{AppSlug: "iterion-forge"}, "iterion-forge[bot]", true},
		{"forgejo app slug is the bot", webhooks.ProviderForgejo, forge.Connection{AppSlug: "iterion-forge"}, "iterion-forge[bot]", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newWebhookTestServer(t)
			conns := forge.NewMemoryConnectionStore()
			tc.conn.ID = "c1"
			tc.conn.TenantID = "t1"
			tc.conn.Provider = forge.Provider(tc.provider)
			if err := conns.Create(context.Background(), tc.conn); err != nil {
				t.Fatal(err)
			}
			s.forgeConnections = conns
			cfg := webhooks.Config{ID: "w1", TenantID: "t1", Provider: tc.provider, ProvisionedBy: "forge:c1"}

			asAuthor := s.realIterionBotAuthor(context.Background(), cfg, tc.login)
			asReviewTarget := s.realIterionBotReviewRequest(context.Background(), cfg, func(l string) bool { return l == tc.login })
			if asAuthor != tc.isBot || asReviewTarget != tc.isBot {
				t.Fatalf("identity asymmetry: author=%v reviewTarget=%v want both %v", asAuthor, asReviewTarget, tc.isBot)
			}
		})
	}
}

// End-to-end shape of the asymmetry's worst case, with a REAL connection
// store and no seams: on a GitHub OAuth connection to a human maintainer's
// account, a collaborator clicking "Request review → that maintainer" is an
// ordinary human gesture — it must never launch a bot run.
func TestGitHubWebhook_HumanAccountConnectionNeverTreatedAsBot(t *testing.T) {
	s := newWebhookTestServer(t)
	conns := forge.NewMemoryConnectionStore()
	if err := conns.Create(context.Background(), forge.Connection{
		ID: "c1", TenantID: "t1", Provider: "github", Kind: "oauth_app", AccountLogin: "alice-maintainer",
	}); err != nil {
		t.Fatal(err)
	}
	s.forgeConnections = conns
	var calls int
	s.webhookLaunchBot = func(_ context.Context, _ string, _ map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		calls++
		return "run-x", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.ProvisionedBy = "forge:c1"

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghReviewRequested("mallory", "alice-maintainer", "2026-09-01T10:00:00Z"), prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusOK || calls != 0 {
		t.Fatalf("human-account review request launched a bot: code=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}
}
