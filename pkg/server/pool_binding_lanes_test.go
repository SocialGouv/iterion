package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The pool: block on the forge-integration surface (ADR-164, #3122): the
// same strict decoder and consent check as the schedule lane, the org
// approval parks and REPLAYS it — re-validating the consent at apply time —
// and the webhook lane folds the provisioned block as its binding layer.

func TestDecodePoolBlock(t *testing.T) {
	if b, spoke, err := decodePoolBlock(nil); b != nil || spoke || err != nil {
		t.Errorf("absent = (%v, %v, %v), want (nil, false, nil) — the request said nothing", b, spoke, err)
	}
	if b, spoke, err := decodePoolBlock(json.RawMessage(` null `)); b != nil || !spoke || err != nil {
		t.Errorf("null = (%v, %v, %v), want (nil, true, nil) — a spoken clear", b, spoke, err)
	}
	b, spoke, err := decodePoolBlock(json.RawMessage(`{"pool_id":"p1","enabled":true,"strategy":{"mode":"priority","margin_pct":70}}`))
	if err != nil || !spoke || b == nil || b.PoolID != "p1" || !b.Enabled || b.Strategy.MarginPct == nil || *b.Strategy.MarginPct != 70 {
		t.Errorf("object = (%+v, %v, %v), want the decoded block", b, spoke, err)
	}
	// A camelCase typo must refuse: a lenient decode would store a block
	// whose margin silently reads as the default.
	if _, _, err := decodePoolBlock(json.RawMessage(`{"pool_id":"p1","enabled":true,"strategy":{"marginPct":70}}`)); err == nil {
		t.Error("a misspelled strategy dial decoded — the strict decode is what keeps a typo from storing a no-op")
	}
}

func TestExpandsProvisionSurface_ASpokenPoolBlockParks(t *testing.T) {
	ri := forge.RepoIntegration{BotIDs: []string{"review-pr"}}
	for _, raw := range []string{`{"pool_id":"p1","enabled":true}`, `null`} {
		if !expandsProvisionSurface(ri, forgeUpdateReq{BotIDs: ri.BotIDs, Pool: json.RawMessage(raw)}) {
			t.Errorf("pool %s does not park — the block moves which credentials fund the repo's unattended runs", raw)
		}
	}
	if expandsProvisionSurface(ri, forgeUpdateReq{BotIDs: ri.BotIDs}) {
		t.Error("an absent pool block parks — a request that never spoke about the pool must go direct")
	}
}

// approvalPoolServer is the approval harness with the pool stores wired and
// org o1 owning an enabled pool.
func approvalPoolServer(t *testing.T) (*Server, string, func()) {
	t.Helper()
	s, _, done := newApprovalTestServer(t)
	pools := credpool.NewMemoryPoolStore()
	s.credPoolPools = pools
	if err := pools.Upsert(context.Background(), credpool.Pool{
		ID: "pool-o1", OrgID: "o1", Name: "o1 forfaits", Enabled: true,
		OwnerType: credpool.PoolOwnerOrg, OwnerID: "o1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := pools.Upsert(context.Background(), credpool.Pool{
		ID: "pool-other", OrgID: "o2", Name: "another org's", Enabled: true,
		OwnerType: credpool.PoolOwnerOrg, OwnerID: "o2",
	}); err != nil {
		t.Fatal(err)
	}
	return s, firstConnID(t, s), done
}

func TestEnableForgeRepoBots_RefusesAPoolBlockWithoutConsent(t *testing.T) {
	s, connID, done := approvalPoolServer(t)
	defer done()
	for name, block := range map[string]string{
		"another org's pool": `{"pool_id":"pool-other","enabled":true}`,
		"an unknown pool":    `{"pool_id":"pool-gone","enabled":true}`,
		"a misspelled dial":  `{"pool_id":"pool-o1","enabled":true,"strategy":{"resetHold":true}}`,
	} {
		w := httptest.NewRecorder()
		s.handleEnableForgeRepoBots(w, forgeReq(teamAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots",
			`{"connection_id":"`+connID+`","repo":"group/api","bot_ids":["review-pr"],"pool":`+block+`}`, "t1"))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: code=%d body=%s, want 400 before anything is parked or provisioned", name, w.Code, w.Body.String())
		}
	}
	if list, _ := s.provisionApprovals.ListByOrg(context.Background(), "o1"); len(list) != 0 {
		t.Errorf("a refused pool block still parked %d request(s)", len(list))
	}
}

func TestParkedProvision_ReplaysThePoolBlock(t *testing.T) {
	s, connID, done := approvalPoolServer(t)
	defer done()

	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(teamAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots",
		`{"connection_id":"`+connID+`","repo":"group/api","bot_ids":["review-pr"],"pool":{"pool_id":"pool-o1","enabled":true}}`, "t1"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("enable should park: code=%d body=%s", w.Code, w.Body.String())
	}
	approvalID := approvalIDFrom(t, w)
	parked, err := s.provisionApprovals.Get(context.Background(), approvalID)
	if err != nil {
		t.Fatal(err)
	}
	want := &credpool.Binding{PoolID: "pool-o1", Enabled: true}
	if !parked.PoolSet || !parked.Pool.Equal(want) {
		t.Fatalf("the parked record lost the pool block: set=%v pool=%+v", parked.PoolSet, parked.Pool)
	}

	w = httptest.NewRecorder()
	req := orgReq(orgAdminCtx(), "POST", "/api/orgs/o1/provision-approvals/"+approvalID+"/approve", "", "o1")
	req.SetPathValue("approval_id", approvalID)
	s.handleApproveProvision(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("approve: code=%d body=%s", w.Code, w.Body.String())
	}
	var res forge.ProvisionResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.IntegrationID == "" {
		t.Fatalf("approve result: %s (%v)", w.Body.String(), err)
	}
	integ, err := s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := s.webhookConfigs.Get(context.Background(), res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	if !integ.Pool.Equal(want) || !cfg.Pool.Equal(want) {
		t.Errorf("the approved block did not reach both records: integration=%+v webhook=%+v", integ.Pool, cfg.Pool)
	}
}

// The consent read at park time is not the consent at apply time: the
// pool's owner disables the pool while the request waits — the org admin's
// approval must not grant it.
func TestParkedProvision_ApplyRevalidatesThePoolConsent(t *testing.T) {
	s, connID, done := approvalPoolServer(t)
	defer done()

	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(teamAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots",
		`{"connection_id":"`+connID+`","repo":"group/api","bot_ids":["review-pr"],"pool":{"pool_id":"pool-o1","enabled":true}}`, "t1"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("enable should park: code=%d body=%s", w.Code, w.Body.String())
	}
	approvalID := approvalIDFrom(t, w)

	off := false
	if _, err := s.credPoolPools.Patch(context.Background(), "pool-o1", credpool.PoolPatch{Enabled: &off}); err != nil {
		t.Fatal(err)
	}

	w = httptest.NewRecorder()
	req := orgReq(orgAdminCtx(), "POST", "/api/orgs/o1/provision-approvals/"+approvalID+"/approve", "", "o1")
	req.SetPathValue("approval_id", approvalID)
	s.handleApproveProvision(w, req)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "no longer admissible") {
		t.Fatalf("approve over a revoked pool consent: code=%d body=%s, want 409 naming the inadmissible block", w.Code, w.Body.String())
	}
	if _, err := s.forgeIntegrations.GetByConnRepo(context.Background(), "t1", connID, "group/api"); err == nil {
		t.Fatal("the refused approval still provisioned the integration")
	}
}

// wirePoolFold wires the stores the launch fold reads onto a server.
func wirePoolFold(t *testing.T, s *Server) {
	t.Helper()
	pools := credpool.NewMemoryPoolStore()
	s.credPoolPools = pools
	s.credPoolAttachments = credpool.NewMemoryAttachmentStore()
	s.credPoolPledges = credpool.NewMemoryPledgeStore()
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s.credPool = credpool.NewBroker(credpool.BrokerConfig{
		Pools: pools, Pledges: s.credPoolPledges, Leases: credpool.NewMemoryLeaseStore(), Ledger: credpool.NewMemoryLedger(),
		OAuth: secrets.NewMemoryOAuthStore(), APIKeys: secrets.NewMemoryApiKeyStore(), Sealer: sealer,
		Logger: iterlog.New(iterlog.LevelError, nil),
	})
	if err := pools.Upsert(context.Background(), credpool.Pool{ID: "pool-bound", OrgID: "o1", Enabled: true, OwnerType: credpool.PoolOwnerOrg, OwnerID: "o1"}); err != nil {
		t.Fatal(err)
	}
}

// The webhook lane folds the provisioned block as its BINDING layer — and,
// as the control, launches with no attachment when the config binds none.
// The schedule lane rides beside it: both record kinds go through the one
// Layer projection.
func TestThePoolBlockReachesTheLaunchFold(t *testing.T) {
	bound := &credpool.Binding{PoolID: "pool-bound", Enabled: true}

	t.Run("webhook", func(t *testing.T) {
		pub := &tierPublisher{}
		s, _ := newTeamForkServer(t, pub)
		wirePoolFold(t, s)
		if _, err := s.launchWebhookBot(boundedCtx(t), webhookLaunch(nil, ""), webhooks.Config{ID: "wh-1", TenantID: "t1", Pool: bound},
			"probe", map[string]string{}, "", "", "acme/repo", nil, nil, store.RunTrustDefault, "", ""); err != nil {
			t.Fatalf("launchWebhookBot = %v", err)
		}
		got := pub.only(t).PoolAttachment
		if got == nil || got.PoolID != "pool-bound" || got.ScopeID != "binding:webhook" {
			t.Fatalf("webhook launch pool attachment = %+v, want pool-bound from the binding:webhook layer", got)
		}
	})

	t.Run("webhook without a block (control)", func(t *testing.T) {
		pub := &tierPublisher{}
		s, _ := newTeamForkServer(t, pub)
		wirePoolFold(t, s)
		if _, err := s.launchWebhookBot(boundedCtx(t), webhookLaunch(nil, ""), webhooks.Config{ID: "wh-1", TenantID: "t1"},
			"probe", map[string]string{}, "", "", "acme/repo", nil, nil, store.RunTrustDefault, "", ""); err != nil {
			t.Fatalf("launchWebhookBot = %v", err)
		}
		if got := pub.only(t).PoolAttachment; got != nil {
			t.Fatalf("a config binding no pool launched with attachment %+v — the block, not the harness, is what the first case proves", got)
		}
	})

	t.Run("schedule", func(t *testing.T) {
		pub := &tierPublisher{}
		s, _ := newTeamForkServer(t, pub)
		wirePoolFold(t, s)
		if err := s.launchScheduledBot(boundedCtx(t), cloudsched.ScheduledBot{
			ID: "sched-1", TenantID: "t1", BotID: "probe", Cron: "0 2 * * 1", Pool: bound,
		}); err != nil {
			t.Fatalf("launchScheduledBot = %v", err)
		}
		got := pub.only(t).PoolAttachment
		if got == nil || got.PoolID != "pool-bound" || got.ScopeID != "binding:schedule" {
			t.Fatalf("schedule launch pool attachment = %+v, want pool-bound from the binding:schedule layer", got)
		}
	})
}

// TestRepoBotsPool_DirectLaneRoundTrips: with no org approval the enable
// stores the block on BOTH records, a PATCH replaces and clears it, a
// silent PATCH keeps it — and the integration's own PATCH (the sync
// toggle) is not a pool surface: its read-modify-write preserves the block
// and its request cannot carry one.
func TestRepoBotsPool_DirectLaneRoundTrips(t *testing.T) {
	gl := newMockGitLab()
	srv := gl.server()
	defer srv.Close()
	s := newForgeTestServer(t)
	ctx := context.Background()
	seedOrg(t, s, "o1", "o1")
	if _, err := s.authStore().CreateTeam(ctx, identity.Team{ID: "t1", Name: "t1", Slug: "t1", OrgID: "o1", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	pools := credpool.NewMemoryPoolStore()
	s.credPoolPools = pools
	for _, id := range []string{"pool-a", "pool-b"} {
		if err := pools.Upsert(ctx, credpool.Pool{ID: id, OrgID: "o1", Enabled: true, OwnerType: credpool.PoolOwnerOrg, OwnerID: "o1"}); err != nil {
			t.Fatal(err)
		}
	}

	w := httptest.NewRecorder()
	s.handleConnectForge(w, forgeReq(superAdminCtx(), "POST", "/api/teams/t1/forge/connections",
		`{"provider":"gitlab","mode":"pat","forge_base_url":"`+srv.URL+`","pat":"glpat-token"}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("connect: code=%d body=%s", w.Code, w.Body.String())
	}
	var connResp forgeConnectResp
	if err := json.Unmarshal(w.Body.Bytes(), &connResp); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(superAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots",
		`{"connection_id":"`+connResp.Connection.ID+`","repo":"group/api","bot_ids":["review-pr"],"pool":{"pool_id":"pool-a","enabled":true}}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("enable: code=%d body=%s", w.Code, w.Body.String())
	}
	var res forge.ProvisionResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	stored := func(phase string, want *credpool.Binding) {
		t.Helper()
		integ, err := s.forgeIntegrations.Get(ctx, res.IntegrationID)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := s.webhookConfigs.Get(ctx, res.WebhookID)
		if err != nil {
			t.Fatal(err)
		}
		if !integ.Pool.Equal(want) || !cfg.Pool.Equal(want) {
			t.Fatalf("%s: integration pool %+v, webhook pool %+v, want %+v on both", phase, integ.Pool, cfg.Pool, want)
		}
	}
	stored("enable", &credpool.Binding{PoolID: "pool-a", Enabled: true})

	if w := patchRepoBots(s, res.IntegrationID, `{"pool":{"pool_id":"pool-b","enabled":true}}`); w.Code != http.StatusOK {
		t.Fatalf("patch replace: code=%d body=%s", w.Code, w.Body.String())
	}
	stored("patch replace", &credpool.Binding{PoolID: "pool-b", Enabled: true})

	if w := patchRepoBots(s, res.IntegrationID, `{}`); w.Code != http.StatusOK {
		t.Fatalf("silent patch: code=%d body=%s", w.Code, w.Body.String())
	}
	stored("silent patch keeps", &credpool.Binding{PoolID: "pool-b", Enabled: true})

	// The integration's own PATCH: its read-modify-write keeps the block,
	// and a pool field in its body is not its to apply.
	w = httptest.NewRecorder()
	r := forgeReq(superAdminCtx(), "PATCH", "/api/teams/t1/forge/integrations/"+res.IntegrationID,
		`{"sync_issues_enabled":true,"pool":{"pool_id":"pool-a","enabled":true}}`, "t1")
	r.SetPathValue("iid", res.IntegrationID)
	s.handlePatchForgeIntegration(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("sync toggle: code=%d body=%s", w.Code, w.Body.String())
	}
	stored("the sync toggle is not a pool surface", &credpool.Binding{PoolID: "pool-b", Enabled: true})

	if w := patchRepoBots(s, res.IntegrationID, `{"pool":null}`); w.Code != http.StatusOK {
		t.Fatalf("patch clear: code=%d body=%s", w.Code, w.Body.String())
	}
	stored("patch clear", nil)
}

// TestCardPoolLayers_theIntegrationSpeaksOnlyForItsBotsWhileEnabled: a card
// names its integration through a member-writable bot-arg and can be
// re-bound to another bot; the integration's pool block (an opt-out
// included) speaks for the bots it runs, while it is enabled.
func TestCardPoolLayers_theIntegrationSpeaksOnlyForItsBotsWhileEnabled(t *testing.T) {
	cfg := &webhooks.Config{ID: "wh", TenantID: "t1", Enabled: true, BotIDs: []string{"review-pr"},
		Pool: &credpool.Binding{PoolID: "pool-a", Enabled: true}}
	if got := cardPoolLayers(cfg, "review-pr"); len(got) != 1 || got[0].PoolID != "pool-a" || got[0].Source != "webhook" {
		t.Fatalf("an allowed bot of an enabled integration = %+v, want the webhook layer", got)
	}
	if got := cardPoolLayers(cfg, "app-dev"); got != nil {
		t.Fatalf("a card re-bound to a bot the integration does not run drew its pool layer %+v", got)
	}
	off := *cfg
	off.Enabled = false
	if got := cardPoolLayers(&off, "review-pr"); got != nil {
		t.Fatalf("a disabled integration still speaks for the card: %+v", got)
	}
	optOut := *cfg
	optOut.Pool = &credpool.Binding{PoolID: "pool-a", Enabled: false}
	if got := cardPoolLayers(&optOut, "review-pr"); len(got) != 1 || got[0].Enabled {
		t.Fatalf("the integration's opt-out did not reach the card lane as a disabled layer: %+v", got)
	}
	if cardPoolLayers(nil, "review-pr") != nil {
		t.Fatal("a card naming no integration has no pool layer")
	}
}

// B: a team that funds its own runs still parks when THIS request binds the
// repo to a shared pool — those runs are funded by the pool, the very thing
// the shared_credentials scope reviews. An opt-out block binds nothing and
// keeps the funded team's direct lane.
func TestApprovalScope_sharedCredentialsParksAFundedTeamBindingAPool(t *testing.T) {
	s, done := scopedApprovalServer(t, identity.ProvisionApprovalSharedCredentials)
	defer done()
	seedTeamKey(t, s, "t1")
	pools := credpool.NewMemoryPoolStore()
	s.credPoolPools = pools
	if err := pools.Upsert(context.Background(), credpool.Pool{ID: "pool-o1", OrgID: "o1", Enabled: true, OwnerType: credpool.PoolOwnerOrg, OwnerID: "o1"}); err != nil {
		t.Fatal(err)
	}
	conn := firstConnID(t, s)
	bind := `{"connection_id":"` + conn + `","repo":"group/api","bot_ids":["review-pr"],"pool":{"pool_id":"pool-o1","enabled":true}}`
	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(teamAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", bind, "t1"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("a funded team binding a live shared pool: code=%d body=%s, want 202 (parked for review)", w.Code, w.Body.String())
	}
	optOut := `{"connection_id":"` + conn + `","repo":"group/api","bot_ids":["review-pr"],"pool":{"pool_id":"pool-o1","enabled":false}}`
	w = httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(teamAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", optOut, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("a funded team opting a repo OUT of a pool: code=%d body=%s, want 200 (direct — it binds nothing shared)", w.Code, w.Body.String())
	}
}

// G: an opt-out block grants nothing, so it needs no consent — naming the
// platform's instance pool, a disabled pool or one that no longer exists
// must write (the fold honours it); an ENABLED block keeps every check.
// I: a user pool's audience may admit a team outside any org by name.
func TestValidatePoolBindingWrite_optOutNeedsNoConsentAndAudienceMayNameATeam(t *testing.T) {
	s, _ := poolRoutesServer(t)
	ctx := context.Background()
	pools := s.credPoolPools.(*credpool.MemoryPoolStore)
	for _, p := range []credpool.Pool{
		{ID: "pool-platform", Enabled: true, OwnerType: credpool.PoolOwnerPlatform},
		{ID: "pool-off", OrgID: "org1", Enabled: false, OwnerType: credpool.PoolOwnerOrg, OwnerID: "org1"},
		{ID: "pool-teams", Enabled: true, OwnerType: credpool.PoolOwnerUser, OwnerID: "jo", Audience: credpool.Audience{Teams: []string{"t-noorg"}}},
	} {
		if err := pools.Upsert(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"pool-platform", "pool-off", "pool-gone"} {
		if err := s.validatePoolBindingWrite(ctx, "team1", &credpool.Binding{PoolID: id, Enabled: false}); err != nil {
			t.Errorf("opt-out of %s refused: %v — an opt-out grants nothing", id, err)
		}
		if err := s.validatePoolBindingWrite(ctx, "team1", &credpool.Binding{PoolID: id, Enabled: true}); err == nil {
			t.Errorf("binding %s ENABLED was accepted — only the opt-out skips the consent", id)
		}
	}
	if _, err := s.authStore().CreateTeam(ctx, identity.Team{ID: "t-noorg", Name: "t-noorg", Slug: "t-noorg", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.validatePoolBindingWrite(ctx, "t-noorg", &credpool.Binding{PoolID: "pool-teams", Enabled: true}); err != nil {
		t.Errorf("a user pool whose audience names the org-less team refused it: %v", err)
	}
	if err := s.validatePoolBindingWrite(ctx, "team1", &credpool.Binding{PoolID: "pool-teams", Enabled: true}); err == nil {
		t.Error("a team the user pool's audience does not name was admitted")
	}
}
