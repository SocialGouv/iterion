package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/webhooks"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The legacy issue-label election refuses a winner the webhook's scope does
// not allow: an Invalid delivery row and a 403 naming the bot. Two label bots
// on one webhook is that case (#3530) — the election falls through to the
// reviewer, which the scope leaves out. The refusal is part of the legacy
// contract every unflipped integration keeps: status, body and audit row are
// pinned here to the byte.
func TestSelectIssueLabeledBot_RefusalKeepsItsBytes(t *testing.T) {
	s := &Server{webhookDeliveries: webhooks.NewMemoryDeliveryStore()}
	cfg := webhooks.Config{ID: "ghw", TenantID: "t1", Provider: webhooks.ProviderGitHub, BotIDs: []string{"nestor", "saboteur"}}
	meta := webhookEventMeta{Kind: "issues", Action: "labeled", ProjectPath: "acme/widgets", SubjectID: "issue:42"}

	rec := httptest.NewRecorder()
	bot, ok := s.selectIssueLabeledBot(context.Background(), rec, cfg, meta, "payload-hash", "203.0.113.7")
	if ok || bot != "" {
		t.Fatalf("selectIssueLabeledBot = (%q, %v), want (\"\", false): the reviewer is outside the webhook's scope", bot, ok)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	const wantBody = `{"error":"bot \"review-pr\" not permitted by this webhook"}` + "\n"
	if got := rec.Body.String(); got != wantBody {
		t.Fatalf("body = %q, want %q", got, wantBody)
	}
	rows, err := s.webhookDeliveries.ListByWebhook(context.Background(), "t1", "ghw", 10)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the refusal recorded %d rows, want 1: %+v", len(rows), rows)
	}
	d := rows[0]
	if d.Status != webhooks.StatusInvalid || d.Error != "bot not permitted by webhook scope" || d.BotID != "" || d.RunID != "" ||
		d.EventKind != "issues" || d.EventAction != "labeled" || d.SubjectID != "issue:42" || d.PayloadHash != "payload-hash" {
		t.Fatalf("the refusal's audit row = %+v, want an invalid row carrying the event and \"bot not permitted by webhook scope\"", d)
	}
}

// The election reads the DEPLOYMENT's role bots, not the built-in names: a
// platform that renamed its implementer and its reviewer elects those.
func TestSelectIssueLabeledBot_ElectsTheDeploymentsRoleBots(t *testing.T) {
	roles := platformcfg.NewMemoryStore[platformcfg.BotRoles]()
	implementer, reviewer := "builder", "critic"
	if err := roles.Put(context.Background(), platformcfg.BotRoles{Implementer: &implementer, Reviewer: &reviewer}); err != nil {
		t.Fatalf("store the role bots: %v", err)
	}
	s := New(Config{SkipProjectRegistration: true, BotRolesSettings: roles}, iterlog.New(iterlog.LevelError, nil))
	s.webhookDeliveries = webhooks.NewMemoryDeliveryStore()
	meta := webhookEventMeta{Kind: "issues", Action: "labeled"}

	for _, c := range []struct {
		name    string
		cfg     webhooks.Config
		wantBot string
		wantOK  bool
	}{
		{"the renamed implementer wins when permitted", webhooks.Config{BotIDs: []string{"critic", "builder", featureDevBotID}}, "builder", true},
		{"the renamed reviewer answers two label bots", webhooks.Config{BotIDs: []string{"nestor", "saboteur", "critic"}}, "critic", true},
		// The built-in implementer holds no role here: it is one bot among
		// several, and the renamed reviewer is outside the scope.
		{"the built-in names hold no role", webhooks.Config{BotIDs: []string{featureDevBotID, defaultWebhookBotReviewPR}}, "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			bot, ok := s.selectIssueLabeledBot(context.Background(), rec, c.cfg, meta, "hash", "203.0.113.7")
			if bot != c.wantBot || ok != c.wantOK {
				t.Fatalf("selectIssueLabeledBot(bots %v) = (%q, %v), want (%q, %v); answer %d %s", c.cfg.BotIDs, bot, ok, c.wantBot, c.wantOK, rec.Code, rec.Body.String())
			}
			if !c.wantOK {
				const wantBody = `{"error":"bot \"critic\" not permitted by this webhook"}` + "\n"
				if rec.Code != http.StatusForbidden || rec.Body.String() != wantBody {
					t.Fatalf("refusal = %d %q, want 403 %q", rec.Code, rec.Body.String(), wantBody)
				}
			}
		})
	}
}
