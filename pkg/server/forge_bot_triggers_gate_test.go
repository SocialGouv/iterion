package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// R-F2: the org gate judges a bot_triggers update by what the PLAN starts.
// The residual couples the bots, so an entry no per-entry reading calls an
// expansion — a residual pin dropped, one bot narrowed — hands labels to a
// bot that did not start on them. A team admin's PATCH then parks; one that
// moves no label goes through.
//
// A wildcard moves labels the same way: alpha back on its manifest's
// "*-urgent" starts on the "p0-urgent" its residual row left to beta, and
// alpha back on "p0-urgent" starts on what beta's "*-urgent" held back.
//
// Mutations: updateExpandsSurface returns expandsProvisionSurface alone (no
// plan comparison) → red on "want 202 parked"; claimGains compares a
// residual row turned explicit to its exclusions by labelKey alone → red on
// the two wildcard cases.
func TestForgeRepoBots_BotTriggersTransferringALabelParks(t *testing.T) {
	cases := []struct {
		name   string
		stored map[string]forge.BotTrigger
		patch  string
		parks  bool
		// manifest is each bot's issue_labeled labels; nil declares x for
		// both.
		manifest map[string][]string
	}{
		{
			name:   "a residual pin dropped hands x to alpha",
			stored: map[string]forge.BotTrigger{"alpha": {Residual: true}},
			patch:  `{"bot_triggers":{"alpha":{}}}`,
			parks:  true,
		},
		{
			name: "alpha narrowed hands deploy-prod to the residual winner",
			stored: map[string]forge.BotTrigger{
				"alpha": {Labels: []string{"secu", "deploy-prod"}},
				"beta":  {Residual: true},
			},
			patch: `{"bot_triggers":{"alpha":{"labels":["secu"]}}}`,
			parks: true,
		},
		{
			name:     "a residual pin dropped puts alpha on *-urgent, over beta's p0-urgent",
			stored:   map[string]forge.BotTrigger{"alpha": {Residual: true}},
			patch:    `{"bot_triggers":{"alpha":{}}}`,
			parks:    true,
			manifest: map[string][]string{"alpha": {"*-urgent"}, "beta": {"p0-urgent"}},
		},
		{
			name:     "a residual pin dropped puts alpha on p0-urgent, under beta's *-urgent",
			stored:   map[string]forge.BotTrigger{"alpha": {Residual: true}},
			patch:    `{"bot_triggers":{"alpha":{}}}`,
			parks:    true,
			manifest: map[string][]string{"alpha": {"p0-urgent"}, "beta": {"*-urgent"}},
		},
		{
			name:     "a residual pin dropped puts alpha on docs, which it served",
			stored:   map[string]forge.BotTrigger{"alpha": {Residual: true}},
			patch:    `{"bot_triggers":{"alpha":{}}}`,
			parks:    false,
			manifest: map[string][]string{"alpha": {"docs"}, "beta": {"p0-urgent"}},
		},
		{
			name: "alpha's labels restated moves nothing",
			stored: map[string]forge.BotTrigger{
				"alpha": {Labels: []string{"secu", "deploy-prod"}},
				"beta":  {Residual: true},
			},
			patch: `{"bot_triggers":{"alpha":{"labels":["deploy-prod","secu"]}}}`,
			parks: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, done := newApprovalTestServer(t)
			defer done()
			// Two bots whose issue_labeled invocation declares the case's
			// labels. The orchestrator resolves both the provision's and the
			// gate's plans through these lookups.
			s.forgeOrchestrator.Bots = func(context.Context, string, string) (*bundle.ForgeRequirements, error) { return nil, nil }
			s.forgeOrchestrator.Invocations = func(_ context.Context, _ string, bot string) ([]bundle.Invocation, error) {
				labels := []string{"x"}
				if tc.manifest != nil {
					labels = tc.manifest[bot]
				}
				return []bundle.Invocation{{
					Kind:  bundle.InvocationKindForge,
					Forge: &bundle.InvocationForge{Event: bundle.ForgeEventIssueLabeled, Labels: labels},
				}}, nil
			}
			connID := firstConnID(t, s)
			w := httptest.NewRecorder()
			s.handleEnableForgeRepoBots(w, forgeReq(orgAdminCtx(), http.MethodPost, "/api/teams/t1/forge/repo-bots",
				`{"connection_id":"`+connID+`","repo":"group/api","bot_ids":["alpha","beta"]}`, "t1"))
			if w.Code != http.StatusOK {
				t.Fatalf("org admin enable: code=%d body=%s", w.Code, w.Body.String())
			}
			var res forge.ProvisionResult
			if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
				t.Fatal(err)
			}
			ri, err := s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
			if err != nil {
				t.Fatal(err)
			}
			ri.BotTriggers = tc.stored
			if err := s.forgeIntegrations.Update(context.Background(), ri); err != nil {
				t.Fatal(err)
			}
			r := forgeReq(teamAdminCtx(), http.MethodPatch, "/api/teams/t1/forge/repo-bots/"+res.IntegrationID, tc.patch, "t1")
			r.SetPathValue("integration_id", res.IntegrationID)
			w = httptest.NewRecorder()
			s.handleUpdateForgeRepoBots(w, r)
			if tc.parks && w.Code != http.StatusAccepted {
				t.Fatalf("a team admin's PATCH %s on a gated org: code=%d body=%s, want 202 parked — it moves a label to another bot", tc.patch, w.Code, w.Body.String())
			}
			if !tc.parks && w.Code != http.StatusOK {
				t.Fatalf("a team admin's PATCH %s on a gated org: code=%d body=%s, want 200 applied — it moves no label", tc.patch, w.Code, w.Body.String())
			}
		})
	}
}

// R2-F3: the gate fails closed toward approval on a plan it cannot make.
// The stored triggers plan, the updated ones are refused (alpha's catalogue
// gained a second issue route between the gate's two manifest reads, and the
// one bot_triggers label set every row of a bot shares now overlaps itself):
// the gate cannot say what the update starts, so it parks — on an
// integration no lane routes by the spine, whose provision only records the
// refusal and writes the update.
//
// Mutation: updateExpandsSurface answers false on a refused updated plan →
// red on "want 202 parked".
func TestForgeRepoBots_RefusedUpdatedPlanParks(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	var armed atomic.Bool
	var alphaReads atomic.Int32
	route := func(label string) bundle.Invocation {
		return bundle.Invocation{Kind: bundle.InvocationKindForge,
			Forge: &bundle.InvocationForge{Event: bundle.ForgeEventIssueLabeled, Labels: []string{label}}}
	}
	s.forgeOrchestrator.Bots = func(context.Context, string, string) (*bundle.ForgeRequirements, error) { return nil, nil }
	s.forgeOrchestrator.Invocations = func(_ context.Context, _ string, bot string) ([]bundle.Invocation, error) {
		if bot == "alpha" && armed.Load() && alphaReads.Add(1) > 1 {
			return []bundle.Invocation{route("a"), route("b")}, nil
		}
		return []bundle.Invocation{route(bot)}, nil
	}
	connID := firstConnID(t, s)
	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(orgAdminCtx(), http.MethodPost, "/api/teams/t1/forge/repo-bots",
		`{"connection_id":"`+connID+`","repo":"group/api","bot_ids":["alpha","beta"]}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("org admin enable: code=%d body=%s", w.Code, w.Body.String())
	}
	var res forge.ProvisionResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	ri, err := s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	ri.BotTriggers = map[string]forge.BotTrigger{"alpha": {Labels: []string{"a"}}}
	if err := s.forgeIntegrations.Update(context.Background(), ri); err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	r := forgeReq(teamAdminCtx(), http.MethodPatch, "/api/teams/t1/forge/repo-bots/"+res.IntegrationID,
		`{"bot_triggers":{"alpha":{"labels":["a"]}}}`, "t1")
	r.SetPathValue("integration_id", res.IntegrationID)
	w = httptest.NewRecorder()
	s.handleUpdateForgeRepoBots(w, r)
	if alphaReads.Load() < 2 {
		t.Fatalf("alpha's manifest was read %d times by the update, want the gate's two reads", alphaReads.Load())
	}
	if w.Code != http.StatusAccepted {
		t.Fatalf("a team admin's PATCH whose updated plan is refused, on a gated org: code=%d body=%s, want 202 parked", w.Code, w.Body.String())
	}
}

// R-F6: a bot_triggers block the projection cannot honour as written is a
// 400 at the edge — on the update and on the enable route — before anything
// is provisioned or parked.
//
// Mutation: validateBotTriggers returns nil → red on "want 400".
func TestForgeRepoBots_BotTriggersValidatedAtTheEdge(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)
	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(orgAdminCtx(), http.MethodPost, "/api/teams/t1/forge/repo-bots", enableBody(connID), "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("org admin enable: code=%d body=%s", w.Code, w.Body.String())
	}
	var res forge.ProvisionResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	for _, block := range []string{
		`{"stranger":{"labels":["x"]}}`,
		`{"review-pr":{"labels":["x","  "]}}`,
		`{"review-pr":{"launch_vars":{"__iterion_repo_ref":"evil"}}}`,
		`{"review-pr":{"launch_vars":{"__ITERION_KEY_OVERRIDES":"{}"}}}`,
		`{"review-pr":{"launch_vars":{"forge_publish_token":"t"}}}`,
		`{"review-pr":{"launch_vars":{"ITERION_RUN_CAPS":"all"}}}`,
		`{"review-pr":{"launch_vars":{" ":"x"}}}`,
	} {
		r := forgeReq(orgAdminCtx(), http.MethodPatch, "/api/teams/t1/forge/repo-bots/"+res.IntegrationID, `{"bot_triggers":`+block+`}`, "t1")
		r.SetPathValue("integration_id", res.IntegrationID)
		w := httptest.NewRecorder()
		s.handleUpdateForgeRepoBots(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("PATCH bot_triggers %s: code=%d body=%s, want 400", block, w.Code, w.Body.String())
		}
		w = httptest.NewRecorder()
		s.handleEnableForgeRepoBots(w, forgeReq(orgAdminCtx(), http.MethodPost, "/api/teams/t1/forge/repo-bots",
			`{"connection_id":"`+connID+`","repo":"group/api","bot_ids":["review-pr"],"bot_triggers":`+block+`}`, "t1"))
		if w.Code != http.StatusBadRequest {
			t.Errorf("enable with bot_triggers %s: code=%d body=%s, want 400", block, w.Code, w.Body.String())
		}
	}
	ri, err := s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	if ri.BotTriggers != nil {
		t.Fatalf("a refused block was written: %+v", ri.BotTriggers)
	}
	r := forgeReq(orgAdminCtx(), http.MethodPatch, "/api/teams/t1/forge/repo-bots/"+res.IntegrationID,
		`{"bot_triggers":{"review-pr":{"labels":["review"],"launch_vars":{"tone":"dry"}}}}`, "t1")
	r.SetPathValue("integration_id", res.IntegrationID)
	w = httptest.NewRecorder()
	s.handleUpdateForgeRepoBots(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("a valid block: code=%d body=%s, want 200", w.Code, w.Body.String())
	}
}

// R-F9: the sync route answers another team's integration as absent before
// it answers anything about this server's wiring — no projector wired reads
// 404 to a stranger, the not-wired refusal to the integration's own team.
//
// Mutation: SyncSubscriptions checks its projector before the tenant (and
// the route skips its own read) → red on "want 404".
func TestForgeSubscriptionsSync_TenantBeforeWiring(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)
	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(orgAdminCtx(), http.MethodPost, "/api/teams/t1/forge/repo-bots", enableBody(connID), "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("org admin enable: code=%d body=%s", w.Code, w.Body.String())
	}
	var res forge.ProvisionResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.authStore().CreateTeam(ctx, identity.Team{ID: "t2", Name: "t2", Slug: "t2", OrgID: "o1", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.authStore().UpsertMembership(ctx, identity.Membership{UserID: "teamadmin", TeamID: "t2", Role: identity.RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	s.forgeOrchestrator.Subscriptions = nil
	sync := func(team string) *httptest.ResponseRecorder {
		r := forgeReq(teamAdminCtx(), http.MethodPost, "/api/teams/"+team+"/forge/repo-bots/"+res.IntegrationID+"/subscriptions/sync", "", team)
		r.SetPathValue("integration_id", res.IntegrationID)
		w := httptest.NewRecorder()
		s.handleSyncForgeSubscriptions(w, r)
		return w
	}
	if w := sync("t2"); w.Code != http.StatusNotFound {
		t.Fatalf("another team's sync of t1's integration: code=%d body=%s, want 404", w.Code, w.Body.String())
	}
	if w := sync("t1"); w.Code == http.StatusNotFound || !strings.Contains(w.Body.String(), "no subscription projector is wired") {
		t.Fatalf("the owning team's sync with no projector wired: code=%d body=%s, want the not-wired answer", w.Code, w.Body.String())
	}
	// No orchestrator at all: the route's own read answers the stranger.
	s.forgeOrchestrator = nil
	if w := sync("t2"); w.Code != http.StatusNotFound {
		t.Fatalf("another team's sync with no orchestrator wired: code=%d body=%s, want 404", w.Code, w.Body.String())
	}
	if w := sync("t1"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("the owning team's sync with no orchestrator wired: code=%d body=%s, want 503", w.Code, w.Body.String())
	}
}
