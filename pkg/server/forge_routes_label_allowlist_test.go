package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// TestForgeRepoBots_LabelAllowlistSurvivesABotSetUpdate pins the INGRESS for the
// issue-lane narrowing. The studio's Integrations tab PATCHes bot_ids and
// nothing else, which re-provisions the repo — so an allowlist that lived only
// on the webhook config was wiped by a bot toggle, and the failure is
// fail-open: every label dispatches the implementer again, with no error to
// notice. The field is operator-owned on the integration, so an absent one
// keeps the stored set and an explicit one replaces it.
func TestForgeRepoBots_LabelAllowlistSurvivesABotSetUpdate(t *testing.T) {
	gl := newMockGitLab()
	srv := gl.server()
	defer srv.Close()

	s := newForgeTestServer(t)
	ctx := superAdminCtx()

	w := httptest.NewRecorder()
	s.handleConnectForge(w, forgeReq(ctx, "POST", "/api/teams/t1/forge/connections",
		`{"provider":"gitlab","mode":"pat","forge_base_url":"`+srv.URL+`","pat":"glpat-token"}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("connect: code=%d body=%s", w.Code, w.Body.String())
	}
	var connResp forgeConnectResp
	if err := json.Unmarshal(w.Body.Bytes(), &connResp); err != nil {
		t.Fatal(err)
	}

	// Enable with the narrowing: only `implement` dispatches the implementer.
	w = httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(ctx, "POST", "/api/teams/t1/forge/repo-bots",
		`{"connection_id":"`+connResp.Connection.ID+`","repo":"group/api","bot_ids":["review-pr"],"label_allowlist":["implement"]}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("enable: code=%d body=%s", w.Code, w.Body.String())
	}
	var res forge.ProvisionResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	cfg, err := s.webhookConfigs.Get(context.Background(), res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.LabelAllowlist, []string{"implement"}) {
		t.Fatalf("label_allowlist did not reach the webhook config: %v", cfg.LabelAllowlist)
	}

	// The studio's gesture: PATCH the bot set, saying nothing about labels.
	patch := forgeReq(ctx, "PATCH", "/api/teams/t1/forge/repo-bots/"+res.IntegrationID,
		`{"bot_ids":["review-pr"]}`, "t1")
	patch.SetPathValue("integration_id", res.IntegrationID)
	w = httptest.NewRecorder()
	s.handleUpdateForgeRepoBots(w, patch)
	if w.Code != http.StatusOK {
		t.Fatalf("update: code=%d body=%s", w.Code, w.Body.String())
	}
	cfg, err = s.webhookConfigs.Get(context.Background(), res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.LabelAllowlist, []string{"implement"}) {
		t.Errorf("a bot-set update widened the issue lane back to any label: %v", cfg.LabelAllowlist)
	}

	// An explicit list replaces it — the operator still owns the field.
	patch = forgeReq(ctx, "PATCH", "/api/teams/t1/forge/repo-bots/"+res.IntegrationID,
		`{"bot_ids":["review-pr"],"label_allowlist":["ship-it"]}`, "t1")
	patch.SetPathValue("integration_id", res.IntegrationID)
	w = httptest.NewRecorder()
	s.handleUpdateForgeRepoBots(w, patch)
	if w.Code != http.StatusOK {
		t.Fatalf("update with allowlist: code=%d body=%s", w.Code, w.Body.String())
	}
	integ, err := s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(integ.LabelAllowlist, []string{"ship-it"}) {
		t.Errorf("explicit label_allowlist not applied: %v", integ.LabelAllowlist)
	}
}
