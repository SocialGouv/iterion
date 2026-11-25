package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// A forge-bound row is projected from its repo integration (ADR-172): the
// triggers REST refuses to edit or delete it, so no operator write can strip
// its label filter (and leave it routable by a filter nobody set) or tombstone
// what the next projection writes back.
func TestTriggersREST_RefusesForgeBoundRows(t *testing.T) {
	srv := newTriggerTestServer(t)
	ctx := context.Background()
	row := trigger.Subscription{
		ID: "drv-forge-1", BotID: "nestor", Invocation: "forge", Enabled: true,
		Origin: trigger.ForgeOrigin("int-1"), ForgeBinding: trigger.ForgeBindingForIntegration("int-1"),
		Match: trigger.Matcher{AddedLabels: []string{"nestor"}},
	}
	if err := srv.cfg.TriggerStore.Create(ctx, row); err != nil {
		t.Fatal(err)
	}

	put := httptest.NewRequest(http.MethodPut, "/api/v1/triggers/"+row.ID,
		bytes.NewBufferString(`{"bot_id":"nestor","invocation":"forge","match":{}}`))
	put.SetPathValue("id", row.ID)
	rec := httptest.NewRecorder()
	srv.handleUpdateTrigger(rec, put)
	if rec.Code != http.StatusConflict {
		t.Fatalf("an edit of a forge-bound row = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if got, _ := srv.cfg.TriggerStore.Get(ctx, row.ID); len(got.Match.AddedLabels) != 1 {
		t.Fatalf("the refused edit rewrote the row's label filter: %+v", got.Match)
	}

	del := httptest.NewRequest(http.MethodDelete, "/api/v1/triggers/"+row.ID, nil)
	del.SetPathValue("id", row.ID)
	rec = httptest.NewRecorder()
	srv.handleDeleteTrigger(rec, del)
	if rec.Code != http.StatusConflict {
		t.Fatalf("a delete of a forge-bound row = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if _, err := srv.cfg.TriggerStore.Get(ctx, row.ID); err != nil {
		t.Fatalf("the refused delete removed the row: %v", err)
	}
}

// The routing switch answers to the org's approval gate on a flip TO the
// spine — and only there: a team admin of a gated org is refused 409
// approval_required naming the org, nothing written; the org admin passes
// the gate (and meets the switch's own refusal on a server that projects no
// subscription); a flip back to legacy never asks the gate.
func TestForgeRouting_FlipToSpineAnswersToTheOrgGate(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(orgAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", enableBody(connID), "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("org admin enable: code=%d body=%s", w.Code, w.Body.String())
	}
	ints, err := s.forgeIntegrations.ListByTenant(context.Background(), "t1")
	if err != nil || len(ints) != 1 {
		t.Fatalf("integrations: %v (%d)", err, len(ints))
	}
	iid := ints[0].ID

	flip := func(ctx context.Context, mode string) (int, map[string]any) {
		r := forgeReq(ctx, http.MethodPut, "/api/teams/t1/forge/repo-bots/"+iid+"/routing",
			`{"lane":"issues","mode":"`+mode+`"}`, "t1")
		r.SetPathValue("integration_id", iid)
		rec := httptest.NewRecorder()
		s.handleSetForgeRouting(rec, r)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}

	code, body := flip(teamAdminCtx(), webhooks.RoutingSpine)
	if code != http.StatusConflict || body["error"] != "approval_required" || body["org_id"] != "o1" {
		t.Fatalf("a team admin's flip to spine on a gated org: code=%d body=%v, want 409 approval_required naming o1", code, body)
	}
	if got, _ := s.forgeIntegrations.Get(context.Background(), iid); !got.ForgeRouting.IsZero() || got.SpineSince != nil {
		t.Fatalf("the refused flip wrote the switch: %+v since=%v", got.ForgeRouting, got.SpineSince)
	}

	code, body = flip(orgAdminCtx(), webhooks.RoutingSpine)
	if code != http.StatusConflict || body["error"] != forge.RoutingRefusedNoProjector {
		t.Fatalf("the org admin's flip on a server with no projector: code=%d body=%v, want 409 %s — past the gate, refused by the switch", code, body, forge.RoutingRefusedNoProjector)
	}

	code, body = flip(teamAdminCtx(), webhooks.RoutingLegacy)
	if code != http.StatusOK {
		t.Fatalf("a team admin's flip back to legacy: code=%d body=%v, want 200 — a rollback never asks the gate", code, body)
	}
}

// A bot_triggers PATCH widens what starts a bot on a label: on a gated org a
// team admin's PATCH parks, like any other expansion of the automated surface.
func TestForgeRepoBots_BotTriggersPatchParksUnderTheOrgGate(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(orgAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", enableBody(connID), "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("org admin enable: code=%d body=%s", w.Code, w.Body.String())
	}
	ints, _ := s.forgeIntegrations.ListByTenant(context.Background(), "t1")
	iid := ints[0].ID

	r := forgeReq(teamAdminCtx(), http.MethodPatch, "/api/teams/t1/forge/repo-bots/"+iid,
		`{"bot_triggers":{"review-pr":{"labels":["review"]}}}`, "t1")
	r.SetPathValue("integration_id", iid)
	w = httptest.NewRecorder()
	s.handleUpdateForgeRepoBots(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("a team admin's bot_triggers PATCH on a gated org: code=%d body=%s, want 202 parked", w.Code, w.Body.String())
	}
	pending, err := s.provisionApprovals.ListByOrg(context.Background(), "o1")
	if err != nil || len(pending) != 1 || pending[0].BotTriggers["review-pr"].Labels == nil {
		t.Fatalf("the parked request does not carry the bot triggers: %+v (%v)", pending, err)
	}
	if got, _ := s.forgeIntegrations.Get(context.Background(), iid); got.BotTriggers != nil {
		t.Fatalf("the parked PATCH wrote bot_triggers: %+v", got.BotTriggers)
	}
}
