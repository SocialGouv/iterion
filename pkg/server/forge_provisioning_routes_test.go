package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The repo-bots routes are the operator's write surface for the
// integration's routing block (ADR-129): the ONE strict decoder at the
// edge (locks refused, unknown fields refused, literal values only), the
// org-approval gate parking every spoken block, the `*`-cell blast-radius
// WARNING, and a GET that answers the stored block verbatim.

const screenProbeBot = `
agent work:
  model: "claude-opus-5"
  system: p
  permission: ask
  ask: ["Bash(git *)"]

prompt p:
  Do the work.

workflow screen_probe:
  entry: work
  work -> done
`

// seedRoutedIntegration connects a mock forge and provisions review-pr,
// with routing on the enable request when block is non-nil.
func seedRoutedIntegration(t *testing.T, s *Server, srvURL string, block *llmroute.Policy) forge.ProvisionResult {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleConnectForge(w, forgeReq(superAdminCtx(), "POST", "/api/teams/t1/forge/connections",
		`{"provider":"gitlab","mode":"pat","forge_base_url":"`+srvURL+`","pat":"glpat-token"}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("connect: code=%d body=%s", w.Code, w.Body.String())
	}
	var connResp forgeConnectResp
	if err := json.Unmarshal(w.Body.Bytes(), &connResp); err != nil {
		t.Fatal(err)
	}
	body := `{"connection_id":"` + connResp.Connection.ID + `","repo":"group/api","bot_ids":["review-pr"]`
	if block != nil {
		body += `,"routing":` + string(mustJSON(t, block))
	}
	body += `}`
	w = httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(superAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", body, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("enable: code=%d body=%s", w.Code, w.Body.String())
	}
	var res forge.ProvisionResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func patchRepoBots(s *Server, integrationID, body string) *httptest.ResponseRecorder {
	r := forgeReq(superAdminCtx(), "PATCH", "/api/teams/t1/forge/repo-bots/"+integrationID, body, "t1")
	r.SetPathValue("integration_id", integrationID)
	rec := httptest.NewRecorder()
	s.handleUpdateForgeRepoBots(rec, r)
	return rec
}

func storedRouting(t *testing.T, s *Server, res forge.ProvisionResult) (*llmroute.Policy, *llmroute.Policy) {
	t.Helper()
	integ, err := s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := s.webhookConfigs.Get(context.Background(), res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	return integ.Routing, cfg.Routing
}

// TestRepoBotsRouting_RoundTrips: POST then PATCH then GET answer the
// stored block verbatim — the operator's block is a VALUE the record
// carries, not a summary of it.
func TestRepoBotsRouting_RoundTrips(t *testing.T) {
	gl := newMockGitLab()
	srv := gl.server()
	defer srv.Close()
	s := newForgeTestServer(t)

	block := &llmroute.Policy{Overrides: []llmroute.Override{
		{Selector: "judge reviewer_claude", Backend: "claw", Model: "anthropic/glm-5"},
		{Selector: "*", Backend: "claw"},
	}}
	res := seedRoutedIntegration(t, s, srv.URL, block)

	gotInteg, gotCfg := storedRouting(t, s, res)
	if !reflect.DeepEqual(gotInteg, block) {
		t.Fatalf("the enable did not store the block verbatim on the integration: got %+v, want %+v", gotInteg, block)
	}
	if !reflect.DeepEqual(gotCfg, block) {
		t.Fatalf("the enable did not store the block verbatim on the webhook config: got %+v, want %+v", gotCfg, block)
	}

	// GET answers the stored block verbatim.
	w := httptest.NewRecorder()
	s.handleListForgeRepoBots(w, forgeReq(superAdminCtx(), "GET", "/api/teams/t1/forge/repo-bots", "", "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("list: code=%d body=%s", w.Code, w.Body.String())
	}
	var list struct {
		Integrations []forge.RepoIntegration `json:"integrations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Integrations) != 1 || !reflect.DeepEqual(list.Integrations[0].Routing, block) {
		t.Fatalf("GET does not answer the stored block verbatim: %+v", list.Integrations)
	}

	// A PATCH replaces wholesale; `null` clears.
	replacement := &llmroute.Policy{Providers: []string{"anthropic"}}
	w = patchRepoBots(s, res.IntegrationID, `{"routing":`+string(mustJSON(t, replacement))+`}`)
	if w.Code != http.StatusOK {
		t.Fatalf("patch replace: code=%d body=%s", w.Code, w.Body.String())
	}
	gotInteg, gotCfg = storedRouting(t, s, res)
	if !reflect.DeepEqual(gotInteg, replacement) || !reflect.DeepEqual(gotCfg, replacement) {
		t.Fatalf("the replace did not reach both stores: integ %+v, cfg %+v, want %+v", gotInteg, gotCfg, replacement)
	}

	w = patchRepoBots(s, res.IntegrationID, `{"routing":null}`)
	if w.Code != http.StatusOK {
		t.Fatalf("patch clear: code=%d body=%s", w.Code, w.Body.String())
	}
	gotInteg, gotCfg = storedRouting(t, s, res)
	if gotInteg != nil || gotCfg != nil {
		t.Fatalf("the clear left blocks behind: integ %+v, cfg %+v — a cleared pin must stay cleared", gotInteg, gotCfg)
	}

	// And the silent PATCH (no routing field) keeps what is stored.
	w = patchRepoBots(s, res.IntegrationID, `{"routing":`+string(mustJSON(t, block))+`}`)
	if w.Code != http.StatusOK {
		t.Fatalf("re-set: code=%d body=%s", w.Code, w.Body.String())
	}
	w = patchRepoBots(s, res.IntegrationID, `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("silent patch: code=%d body=%s", w.Code, w.Body.String())
	}
	gotInteg, gotCfg = storedRouting(t, s, res)
	if !reflect.DeepEqual(gotInteg, block) || !reflect.DeepEqual(gotCfg, block) {
		t.Fatalf("a PATCH that says nothing about routing dropped the stored block: integ %+v, cfg %+v", gotInteg, gotCfg)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestRepoBotsPatch_RefusesLocks: the binding level holds no lock
// primitive — a lock on the repo's block is a 422 naming the fields, not a
// silent no-op audited as set.
func TestRepoBotsPatch_RefusesLocks(t *testing.T) {
	gl := newMockGitLab()
	srv := gl.server()
	defer srv.Close()
	s := newForgeTestServer(t)
	res := seedRoutedIntegration(t, s, srv.URL, nil)

	w := patchRepoBots(s, res.IntegrationID, `{"routing":{"locks":["pair_order"]}}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("locks: code=%d body=%s, want 422 — a binding-level lock would launder unset past the author", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "locks") {
		t.Fatalf("the refusal does not name the offending field: %s", w.Body.String())
	}
}

// TestRepoBotsPatch_RefusesUnknownField: a camelCase typo must refuse, not
// silently store a no-op block audited as set.
func TestRepoBotsPatch_RefusesUnknownField(t *testing.T) {
	gl := newMockGitLab()
	srv := gl.server()
	defer srv.Close()
	s := newForgeTestServer(t)
	res := seedRoutedIntegration(t, s, srv.URL, nil)

	w := patchRepoBots(s, res.IntegrationID, `{"routing":{"backned":"claw"}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: code=%d body=%s, want 400", w.Code, w.Body.String())
	}
}

// TestRepoBotsPatch_RefusesTemplateValue: an override value is a LITERAL —
// a templated backend would leave the pin unreadable at the launch
// pre-flights, which is exactly what the typed block exists to prevent.
func TestRepoBotsPatch_RefusesTemplateValue(t *testing.T) {
	gl := newMockGitLab()
	srv := gl.server()
	defer srv.Close()
	s := newForgeTestServer(t)
	res := seedRoutedIntegration(t, s, srv.URL, nil)

	w := patchRepoBots(s, res.IntegrationID, `{"routing":{"overrides":[{"selector":"*","backend":"{{vars.b}}"}]}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("template value: code=%d body=%s, want 400 — a templated pin is unreadable at the gate", w.Code, w.Body.String())
	}
}

// TestUpdateWithRouting_ParksForOrgApproval: a routing block moves the
// repo's unattended spend (the binding level outranks team and org), and
// the predicate cannot tell a tightening from a widening at that surface —
// so every SPOKEN block parks, the ScheduleCrons precedent ("fail toward
// approval, never past it"). A silent PATCH keeps going direct.
func TestUpdateWithRouting_ParksForOrgApproval(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(orgAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", enableBody(connID), "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("seed enable: code=%d body=%s", w.Code, w.Body.String())
	}
	var res forge.ProvisionResult
	json.Unmarshal(w.Body.Bytes(), &res)

	block := &llmroute.Policy{Overrides: []llmroute.Override{{Selector: "*", Backend: "claw"}}}
	w = httptest.NewRecorder()
	r := forgeReq(teamAdminCtx(), "PATCH", "/api/teams/t1/forge/repo-bots/"+res.IntegrationID, `{"routing":`+string(mustJSON(t, block))+`}`, "t1")
	r.SetPathValue("integration_id", res.IntegrationID)
	s.handleUpdateForgeRepoBots(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("a spoken routing block must park for org approval: code=%d body=%s", w.Code, w.Body.String())
	}
	// Nothing landed while pending: the record is the durable carrier and
	// it still says nothing.
	gotInteg, gotCfg := storedRouting(t, s, res)
	if gotInteg != nil || gotCfg != nil {
		t.Fatalf("the block landed while the request was pending approval: integ %+v cfg %+v", gotInteg, gotCfg)
	}
	// A silent PATCH is not an expansion — it stays direct.
	w = httptest.NewRecorder()
	r = forgeReq(teamAdminCtx(), "PATCH", "/api/teams/t1/forge/repo-bots/"+res.IntegrationID, `{}`, "t1")
	r.SetPathValue("integration_id", res.IntegrationID)
	s.handleUpdateForgeRepoBots(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("a silent patch must stay direct: code=%d body=%s", w.Code, w.Body.String())
	}
}

// TestRepoBotsPatch_WarnsOnAStarCellThatWouldRefuseACoEnabledBot: the
// write surface cannot see the blast radius of a `*` cell — it screens the
// submitted block against the bots ENABLED on the integration and WARNS,
// naming the bot and the node whose ask-rules the overridden backend could
// not gate. Not a refusal: a bot enabled later cannot be screened here,
// and the launch refusal stays the enforcement.
func TestRepoBotsPatch_WarnsOnAStarCellThatWouldRefuseACoEnabledBot(t *testing.T) {
	gl := newMockGitLab()
	srv := gl.server()
	defer srv.Close()
	s := newForgeTestServer(t)

	botsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(botsDir, "review-pr.bot"), []byte(screenProbeBot), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{botsDir}

	res := seedRoutedIntegration(t, s, srv.URL, nil)

	block := &llmroute.Policy{Overrides: []llmroute.Override{{Selector: "*", Backend: "codex"}}}
	w := patchRepoBots(s, res.IntegrationID, `{"routing":`+string(mustJSON(t, block))+`}`)
	if w.Code != http.StatusOK {
		t.Fatalf("the screen must WARN, not refuse: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		forge.ProvisionResult
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Warnings) == 0 {
		t.Fatalf("no warning for a `*` cell that would refuse review-pr's gated node — the operator saves blind")
	}
	joined := strings.Join(resp.Warnings, "\n")
	if !strings.Contains(joined, "review-pr") {
		t.Errorf("the warning must name the bot: %s", joined)
	}
	if !strings.Contains(joined, "work") {
		t.Errorf("the warning must name the node whose gate the override breaks: %s", joined)
	}
	if !strings.Contains(joined, "codex") {
		t.Errorf("the warning must name the backend that cannot gate: %s", joined)
	}
	// The write itself still landed.
	gotInteg, gotCfg := storedRouting(t, s, res)
	if !reflect.DeepEqual(gotInteg, block) || !reflect.DeepEqual(gotCfg, block) {
		t.Fatalf("the warned write did not land: integ %+v cfg %+v", gotInteg, gotCfg)
	}
}

// TestBlastRadiusScreen_CollectsTheMaterializedBundle: the screen resolves
// each screened bot the way the launch path does, and the launch path defers
// lb.Cleanup() because a stored bot materializes into a temp dir the
// launchBot owns. A screen that skips the collection leaks one
// iterion-launch-bot-* directory per screened bot per spoken routing write.
func TestBlastRadiusScreen_CollectsTheMaterializedBundle(t *testing.T) {
	s := newForgeTestServer(t)
	s.botSources = botsource.NewMemoryStore()
	if _, err := s.botSources.Create(store.WithTenant(context.Background(), "t1"), botsource.BotSource{
		TenantID: "t1",
		Slug:     "screen-probe",
		Files:    map[string]string{botsource.MainBotFile: screenProbeBot},
	}); err != nil {
		t.Fatal(err)
	}
	leaked := func(phase string) int {
		t.Helper()
		entries, err := os.ReadDir(os.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "iterion-launch-bot-") {
				n++
			}
		}
		return n
	}
	before := leaked("before")
	block := &llmroute.Policy{Overrides: []llmroute.Override{{Selector: "*", Backend: "codex"}}}
	warnings := s.routingBlastRadiusWarnings(context.Background(), "t1", []string{"screen-probe"}, block)
	if len(warnings) == 0 {
		t.Fatalf("the screen never exercised the materialization path (no warnings at all): %v", warnings)
	}
	if after := leaked("after"); after > before {
		t.Fatalf("LEAK: %d iterion-launch-bot-* dir(s) left behind (before=%d after=%d) — the screen never calls lb.Cleanup()", after-before, before, after)
	}
}

// TestPatchForgeIntegration_RoutingIsNotItsSurface: the sync-toggle PATCH
// is deliberately NOT a routing write surface — the repo-bots PATCH owns
// that block (the one strict decoder, the org-approval gate, the
// blast-radius screen), and this endpoint's decode narrows to its own
// field. The witness pins the no-op so the silence stays a decision: a
// handler that starts decoding `routing` here would bypass the org gate
// this slice routes every spoken block through, and this test reddens.
func TestPatchForgeIntegration_RoutingIsNotItsSurface(t *testing.T) {
	gl := newMockGitLab()
	srv := gl.server()
	defer srv.Close()
	s := newForgeTestServer(t)

	block := llmroute.Policy{Overrides: []llmroute.Override{{Selector: "*", Backend: "claw"}}}
	res := seedRoutedIntegration(t, s, srv.URL, &block)

	w := httptest.NewRecorder()
	r := forgeReq(superAdminCtx(), "PATCH", "/api/teams/t1/forge/integrations/"+res.IntegrationID,
		`{"sync_issues_enabled":true,"routing":{"overrides":[{"selector":"*","backend":"byo"}]}}`, "t1")
	r.SetPathValue("iid", res.IntegrationID)
	s.handlePatchForgeIntegration(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("sync toggle: code=%d body=%s", w.Code, w.Body.String())
	}
	integ, err := s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	if !integ.SyncIssuesEnabled {
		t.Fatal("the endpoint's own field stopped working")
	}
	if integ.Routing == nil || !reflect.DeepEqual(integ.Routing, &block) {
		t.Fatalf("the sync toggle rewrote the routing block (now %+v) — a surface it does not own and a gate it does not pass", integ.Routing)
	}
}
