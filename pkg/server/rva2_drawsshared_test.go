package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// Under shared_credentials, a self-funded team's integration that ALREADY
// binds a shared pool (provisioned by the org admin) gains a new bot through
// a PATCH (or the enable add-bots path) that does not restate the pool: the
// gate reads drawsShared off the REQUEST's block only, so the new bot is
// provisioned directly — and its runs fold the stored block and draw the pool.
func TestPoolReview2_addingABotUnderAStoredSharedPoolBlockSkipsTheReview(t *testing.T) {
	for _, lane := range []string{"PATCH", "enable-add-bots"} {
		t.Run(lane, func(t *testing.T) {
			ctx := context.Background()
			s, done := scopedApprovalServer(t, identity.ProvisionApprovalSharedCredentials)
			defer done()
			seedTeamKey(t, s, "t1")
			// dep-guard provisions like review-pr (a forge: block of its own).
			s.forgeOrchestrator.Bots = func(c context.Context, tenant, botID string) (*bundle.ForgeRequirements, error) {
				if botID == "dep-guard" {
					botID = "review-pr"
				}
				return testForgeBotLookup(c, tenant, botID)
			}
			pools := credpool.NewMemoryPoolStore()
			s.credPoolPools = pools
			if err := pools.Upsert(ctx, credpool.Pool{
				ID: "pool-o1", OrgID: "o1", Name: "org forfaits + paid gateway", Enabled: true,
				OwnerType: credpool.PoolOwnerOrg, OwnerID: "o1",
			}); err != nil {
				t.Fatal(err)
			}
			connID := firstConnID(t, s)

			// The org admin provisions review-pr bound to the org pool (reviewed by right).
			w := httptest.NewRecorder()
			s.handleEnableForgeRepoBots(w, forgeReq(orgAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots",
				`{"connection_id":"`+connID+`","repo":"group/api","bot_ids":["review-pr"],"pool":{"pool_id":"pool-o1","enabled":true}}`, "t1"))
			if w.Code != http.StatusOK {
				t.Fatalf("org admin enable: code=%d body=%s", w.Code, w.Body.String())
			}
			var res struct {
				IntegrationID string `json:"integration_id"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &res)
			ri, err := s.forgeIntegrations.Get(ctx, res.IntegrationID)
			if err != nil {
				t.Fatalf("integration: %v (%s)", err, w.Body.String())
			}

			// The team admin adds dep-guard, saying nothing about the pool.
			w = httptest.NewRecorder()
			if lane == "PATCH" {
				req := forgeReq(teamAdminCtx(), "PATCH", "/api/teams/t1/forge/repo-bots/"+ri.ID, `{"bot_ids":["review-pr","dep-guard"]}`, "t1")
				req.SetPathValue("integration_id", ri.ID)
				s.handleUpdateForgeRepoBots(w, req)
			} else {
				s.handleEnableForgeRepoBots(w, forgeReq(teamAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots",
					`{"connection_id":"`+connID+`","repo":"group/api","bot_ids":["dep-guard"]}`, "t1"))
			}
			parked, _ := s.provisionApprovals.ListByOrg(ctx, "o1")
			ri2, _ := s.forgeIntegrations.Get(ctx, ri.ID)
			cfg, _ := s.webhookConfigs.Get(ctx, ri2.WebhookID)
			t.Logf("%s add dep-guard (no pool field): code=%d parked=%d integration.bots=%v webhook.bots=%v webhook.pool=%+v",
				lane, w.Code, len(parked), ri2.BotIDs, cfg.BotIDs, cfg.Pool)
			if len(parked) == 0 && slices.Contains(cfg.BotIDs, "dep-guard") && cfg.Pool != nil && cfg.Pool.Enabled {
				t.Errorf("dep-guard was provisioned WITHOUT review and its runs fold the stored block drawing %s", cfg.Pool.PoolID)
			}
		})
	}
}

// The generic webhook CRUD twin: the managed config ALREADY carries the
// enabled pool block; a team admin widens it to every bot (wildcard_bots)
// — "the webhook CRUD carries no pool block: it never binds a shared pool"
// passes drawsShared=false, so a self-funded team's widening goes direct.
func TestPoolReview2_webhookCRUDWideningAManagedPoolBoundConfigSkipsTheReview(t *testing.T) {
	ctx := context.Background()
	s, done := scopedApprovalServer(t, identity.ProvisionApprovalSharedCredentials)
	defer done()
	seedTeamKey(t, s, "t1")
	pools := credpool.NewMemoryPoolStore()
	s.credPoolPools = pools
	if err := pools.Upsert(ctx, credpool.Pool{ID: "pool-o1", OrgID: "o1", Enabled: true,
		OwnerType: credpool.PoolOwnerOrg, OwnerID: "o1"}); err != nil {
		t.Fatal(err)
	}
	connID := firstConnID(t, s)
	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(orgAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots",
		`{"connection_id":"`+connID+`","repo":"group/api","bot_ids":["review-pr"],"pool":{"pool_id":"pool-o1","enabled":true}}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("org admin enable: code=%d body=%s", w.Code, w.Body.String())
	}
	var res struct {
		WebhookID string `json:"webhook_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	for _, body := range []string{`{"bot_ids":["review-pr","dep-guard"]}`, `{"wildcard_bots":true}`} {
		rec := httptest.NewRecorder()
		req := forgeReq(teamAdminCtx(), "PATCH", "/api/teams/t1/webhooks/"+res.WebhookID, body, "t1")
		req.SetPathValue("webhook_id", res.WebhookID)
		s.handleUpdateWebhook(rec, req)
		cfg, _ := s.webhookConfigs.Get(ctx, res.WebhookID)
		t.Logf("team admin PATCH %s: code=%d bots=%v wildcard=%v pool=%+v allowsBot(any-bot)=%v",
			body, rec.Code, cfg.BotIDs, cfg.WildcardBots, cfg.Pool, cfg.AllowsBot("any-bot"))
		if rec.Code == http.StatusOK && cfg.Pool != nil && cfg.Pool.Enabled {
			t.Errorf("a self-funded team's admin widened the pool-bound managed config (%s) with no review", body)
		}
	}
}
