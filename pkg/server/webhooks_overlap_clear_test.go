package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/schedgate"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// R6: the clear must trip the org approval gate EXACTLY like a literal
// "allow" — the normalization runs AHEAD of webhookPatchExpandsSurface, or a
// team admin widens a managed webhook skip → allow with no approval in a
// RequireProvisionApproval org. Byte-identical outcome to {"overlap":"allow"}.
//
// Mutation that reddens this test: normalizing below the expansion gate.
func TestWebhookPatchOverlapClearTripsTheApprovalGate(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	// Org admins provision directly: a managed config holding "skip".
	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(orgAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots",
		`{"connection_id":"`+connID+`","repo":"group/api","bot_ids":["review-pr"],"overlap":"skip"}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("enable: code=%d body=%s", w.Code, w.Body.String())
	}
	var res forge.ProvisionResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}

	patch := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.handleUpdateWebhook(rec, whReq(teamAdminCtx(), "PATCH", "/api/teams/t1/webhooks/"+res.WebhookID, body, "t1", res.WebhookID))
		return rec
	}
	cfgUnchanged := func() bool {
		cfg, err := s.webhookConfigs.Get(context.Background(), res.WebhookID)
		return err == nil && cfg.Overlap == "skip"
	}

	// The clear and the literal "allow" are the SAME widening — same answer,
	// nothing written either time.
	clearResp := patch(`{"overlap":""}`)
	literalResp := patch(`{"overlap":"allow"}`)
	if clearResp.Code != http.StatusConflict || literalResp.Code != http.StatusConflict {
		t.Fatalf("clear=%d literal=%d — the clear must trip the gate exactly like a literal allow (409)", clearResp.Code, literalResp.Code)
	}
	if clearResp.Body.String() != literalResp.Body.String() {
		t.Errorf("the two widenings must answer identically:\nclear:   %s\nliteral: %s", clearResp.Body.String(), literalResp.Body.String())
	}
	if !strings.Contains(clearResp.Body.String(), "repo-bots") {
		t.Errorf("the refusal must point at the governed path: %s", clearResp.Body.String())
	}
	if !cfgUnchanged() {
		t.Error("a gated patch wrote anyway")
	}

	// (d) Already at the widened value: no false expansion, the clear is a
	// plain 200 — the gate trips on CHANGE, not on the value.
	cfg, err := s.webhookConfigs.Get(context.Background(), res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Overlap = "allow"
	if err := s.webhookConfigs.Update(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if rec := patch(`{"overlap":""}`); rec.Code != http.StatusOK {
		t.Errorf("clear onto an already-allow config must not trip the gate: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if cfg, err = s.webhookConfigs.Get(context.Background(), res.WebhookID); err != nil || cfg.Overlap != "allow" {
		t.Errorf("the no-op clear must leave allow in place: %v %q", err, cfg.Overlap)
	}
}

// non-empty value survives the provision adopt — stored "", the scalar's
// empty reads as never-set and the next silent re-provision resurrects the
// integration's stale value over the operator's gesture. The clear is
// normalized to "allow" (enforcement-identical: empty means allow, and with
// MaxConcurrent 0 the gate is unlimited either way).
func TestWebhookPatchOverlapClearIsNormalizedToAllow(t *testing.T) {
	s := newWebhookTestServer(t)
	ctx := superAdminCtx()

	w := httptest.NewRecorder()
	s.handleCreateWebhook(w, whReq(ctx, "POST", "/api/teams/t1/webhooks",
		`{"name":"gl","bot_ids":["review-pr"],"overlap":"skip"}`, "t1", ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s", w.Code, w.Body.String())
	}
	var created struct {
		Config webhooks.Config `json:"config"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	w = httptest.NewRecorder()
	s.handleUpdateWebhook(w, whReq(ctx, "PATCH", "/api/teams/t1/webhooks/"+created.Config.ID,
		`{"overlap":""}`, "t1", created.Config.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("patch clear: code=%d body=%s", w.Code, w.Body.String())
	}
	stored, err := s.webhookConfigs.Get(context.Background(), created.Config.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Overlap != schedgate.OverlapAllow {
		t.Fatalf("the documented clear stored %q — \"\" reads as never-set, and the next re-provision resurrects the stale value over it", stored.Overlap)
	}

	// A named policy is still stored as sent, and an omitted field untouched.
	w = httptest.NewRecorder()
	s.handleUpdateWebhook(w, whReq(ctx, "PATCH", "/api/teams/t1/webhooks/"+created.Config.ID,
		`{"overlap":"supersede"}`, "t1", created.Config.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("patch supersede: code=%d body=%s", w.Code, w.Body.String())
	}
	if stored, err = s.webhookConfigs.Get(context.Background(), created.Config.ID); err != nil || stored.Overlap != "supersede" {
		t.Fatalf("a named policy must be stored as sent: %v %q", err, stored.Overlap)
	}
	w = httptest.NewRecorder()
	s.handleUpdateWebhook(w, whReq(ctx, "PATCH", "/api/teams/t1/webhooks/"+created.Config.ID,
		`{"name":"renamed"}`, "t1", created.Config.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("patch name: code=%d body=%s", w.Code, w.Body.String())
	}
	if stored, err = s.webhookConfigs.Get(context.Background(), created.Config.ID); err != nil || stored.Overlap != "supersede" {
		t.Fatalf("an unrelated patch touched overlap: %v %q", err, stored.Overlap)
	}
}

// The round-5 chain end to end: provisioned "skip" on both stores, the
// operator clears via the documented PATCH, a silent re-provision must keep
// "allow" on BOTH stores — never resurrect the stale "skip".
//
// Mutation that reddens this test: storing "" instead of "allow" for the
// explicit clear.
func TestForgeOverlapClearSurvivesReprovision(t *testing.T) {
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

	w = httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(ctx, "POST", "/api/teams/t1/forge/repo-bots",
		`{"connection_id":"`+connResp.Connection.ID+`","repo":"group/api","bot_ids":["review-pr"],"overlap":"skip"}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("enable: code=%d body=%s", w.Code, w.Body.String())
	}
	var res forge.ProvisionResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}

	// The documented clear: PATCH the webhook config's overlap to "".
	w = httptest.NewRecorder()
	s.handleUpdateWebhook(w, whReq(ctx, "PATCH", "/api/teams/t1/webhooks/"+res.WebhookID,
		`{"overlap":""}`, "t1", res.WebhookID))
	if w.Code != http.StatusOK {
		t.Fatalf("patch clear: code=%d body=%s", w.Code, w.Body.String())
	}
	cfg, err := s.webhookConfigs.Get(context.Background(), res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Overlap != schedgate.OverlapAllow {
		t.Fatalf("precondition: the clear normalized to %q, want allow", cfg.Overlap)
	}

	// The silent re-provision (a studio bot-set PATCH says nothing about
	// overlap): the clear must survive on both stores.
	patch := forgeReq(ctx, "PATCH", "/api/teams/t1/forge/repo-bots/"+res.IntegrationID,
		`{"bot_ids":["review-pr"]}`, "t1")
	patch.SetPathValue("integration_id", res.IntegrationID)
	w = httptest.NewRecorder()
	s.handleUpdateForgeRepoBots(w, patch)
	if w.Code != http.StatusOK {
		t.Fatalf("silent re-provision: code=%d body=%s", w.Code, w.Body.String())
	}
	cfg, err = s.webhookConfigs.Get(context.Background(), res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Overlap != schedgate.OverlapAllow {
		t.Errorf("the clear was reverted on the enforcement half: overlap = %q", cfg.Overlap)
	}
	integ, err := s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	if integ.Overlap != schedgate.OverlapAllow {
		t.Errorf("the integration did not converge to the cleared value: overlap = %q", integ.Overlap)
	}
}
