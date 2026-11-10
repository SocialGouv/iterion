package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// RVA-B: under Org.ProvisionApprovalScope=shared_credentials, a team that
// holds ONE key of its own is waved past the approval gate — even when the
// very request binds the repo's unattended runs to a SHARED pool (the org's
// pool here). The scope's own doc: it parks "the ones whose runs would be
// funded by the org tier, the pool, or the platform".
func TestPoolForgeReview_SharedCredentialsScopeWavesAPoolBindingPastReview(t *testing.T) {
	s, done := scopedApprovalServer(t, identity.ProvisionApprovalSharedCredentials)
	defer done()
	seedTeamKey(t, s, "t1") // one Anthropic BYOK key: "funds its own runs"
	pools := credpool.NewMemoryPoolStore()
	s.credPoolPools = pools
	if err := pools.Upsert(context.Background(), credpool.Pool{
		ID: "pool-o1", OrgID: "o1", Name: "org forfaits + paid gateway", Enabled: true,
		OwnerType: credpool.PoolOwnerOrg, OwnerID: "o1",
	}); err != nil {
		t.Fatal(err)
	}
	connID := firstConnID(t, s)

	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(teamAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots",
		`{"connection_id":"`+connID+`","repo":"group/api","bot_ids":["review-pr"],"pool":{"pool_id":"pool-o1","enabled":true}}`, "t1"))
	t.Logf("enable with an org-pool block, shared_credentials scope, funded team: code=%d", w.Code)
	if w.Code == http.StatusOK {
		ri, err := s.forgeIntegrations.GetByConnRepo(context.Background(), "t1", connID, "group/api")
		if err == nil {
			cfg, _ := s.webhookConfigs.Get(context.Background(), ri.WebhookID)
			t.Logf("provisioned WITHOUT review: integration.Pool=%+v webhook.Pool=%+v", ri.Pool, cfg.Pool)
		}
	}
	if list, _ := s.provisionApprovals.ListByOrg(context.Background(), "o1"); len(list) == 0 {
		t.Errorf("no approval parked: the pool block binding the ORG's pool went through unreviewed (code=%d body=%s)", w.Code, w.Body.String())
	}
}

// RVA-C: the card lanes (board dispatch + pipeline admission) fold the
// integration's routing and retry blocks through cardLaunchPolicyLayers —
// "the two layers the DIRECT lane hands its resolvers" — but not its pool
// block, which the direct lane (launchWebhookBot) now hands its pool fold.
// Driven through the real ensureBoardCard -> processBoardCard pair.
func TestPoolForgeReview_CardLaneDropsTheIntegrationsPoolBlock(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block *credpool.Binding
	}{
		{"integration binds pool-bound", &credpool.Binding{PoolID: "pool-bound", Enabled: true}},
		{"integration opts OUT (enabled:false) over a team attachment", &credpool.Binding{PoolID: "pool-bound", Enabled: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// direct lane, for the comparison
			directPub := &tierPublisher{}
			ds, _ := newTeamForkServer(t, directPub)
			wirePoolFold(t, ds)
			seedTeamPoolAttachment(t, ds)
			cfg := webhooks.Config{ID: "wh1", TenantID: "t1", Provider: webhooks.ProviderGitHub, Enabled: true, BotIDs: []string{"probe"}, Pool: tc.block}
			if _, err := ds.launchWebhookBot(boundedCtx(t), webhookLaunch(nil, ""), cfg,
				"probe", map[string]string{}, "", "", "acme/repo", nil, nil, store.RunTrustDefault, "", "", ""); err != nil {
				t.Fatalf("launchWebhookBot = %v", err)
			}
			direct := directPub.only(t).PoolAttachment

			// card lane, same integration
			pub := &tierPublisher{}
			s, rs := newTeamForkServer(t, pub)
			pub.onLaunch = func(runID string) { finishRunAs(t, rs, runID, store.RunStatusFinished) }
			wirePoolFold(t, s)
			seedTeamPoolAttachment(t, s)
			board, err := native.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			s.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
			seedIntegrationConfig(t, s, cfg)
			s.ensureBoardCard(boundedCtx(t), cfg, webhooks.CommandRoute{BotID: "probe", Mode: "board"},
				map[string]string{"pr_url": "https://forge.example/o/r/pull/7"},
				webhookEventMeta{Kind: "note", SubjectID: "note:1", SubjectURL: "https://forge.example/o/r/pull/7"},
				"", "https://forge.example/o/r.git", "main")
			cards, err := board.List(native.ListFilter{})
			if err != nil || len(cards) != 1 || cards[0].BotArgs[boardWebhookIDKey] != "wh1" {
				t.Fatalf("card setup: %v %d", err, len(cards))
			}
			if err := s.processBoardCard(boundedCtx(t), "t1", *cards[0]); err != nil {
				t.Fatalf("processBoardCard = %v", err)
			}
			carded := pub.only(t).PoolAttachment
			t.Logf("direct lane: %+v", direct)
			t.Logf("card lane:   %+v", carded)
			if (direct == nil) != (carded == nil) || (direct != nil && (direct.PoolID != carded.PoolID || direct.ScopeID != carded.ScopeID)) {
				t.Errorf("the same integration's runs draw differently by lane: direct=%+v carded=%+v", direct, carded)
			}
		})
	}
}

// seedTeamPoolAttachment attaches an enabled team-level pool (pool-team)
// so an ignored binding is visible as the team answering instead.
func seedTeamPoolAttachment(t *testing.T, s *Server) {
	t.Helper()
	if err := s.credPoolPools.Upsert(context.Background(), credpool.Pool{ID: "pool-team", OrgID: "o1", Enabled: true, OwnerType: credpool.PoolOwnerOrg, OwnerID: "o1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.credPoolAttachments.Put(context.Background(), credpool.Attachment{ScopeID: credpool.TeamScopeID("t1"), PoolID: "pool-team", Enabled: true}); err != nil {
		t.Fatal(err)
	}
}

// RVA-D: the credential preview's webhook source claims "the pool
// attachment resolves exactly as the launch site does (ADR-164 preview
// parity)", yet it folds none of the webhook config's pool block, which
// launchWebhookBot folds as binding:webhook.
func TestPoolForgeReview_WebhookPreviewIgnoresTheConfigsPoolBlock(t *testing.T) {
	s, rs := newTeamForkServer(t, &tierPublisher{})
	pub := &credentialPreviewPublisher{}
	s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(pub))
	wirePoolFold(t, s)
	configs := webhooks.NewMemoryConfigStore()
	s.webhookConfigs = configs
	cfg := webhooks.Config{ID: "hook", TenantID: "t1", Enabled: true, BotIDs: []string{"probe"}, DefaultBotID: "probe",
		Pool: &credpool.Binding{PoolID: "pool-bound", Enabled: true}}
	if err := configs.Create(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	w := callCredentialPreview(s, `{"source":{"kind":"webhook","id":"hook"}}`, auth.Identity{UserID: "human", TeamID: "t1", Role: identity.RoleMember})
	if w.Code != http.StatusOK {
		t.Fatalf("preview=%d %s", w.Code, w.Body.String())
	}
	ctx := store.WithIdentity(auth.WithIdentity(t.Context(), auth.Identity{UserID: "webhook:hook", TeamID: "t1", Kind: auth.KindWebhook}), "t1", "webhook:hook")
	if _, err := s.launchWebhookBot(ctx, webhookLaunch(nil, ""), cfg, "probe", applyWebhookVarLayers(map[string]string{}, cfg, store.RunTrustDefault), "", "", "", nil, nil, store.RunTrustDefault, "", "", ""); err != nil {
		t.Fatal(err)
	}
	t.Logf("preview pool attachment: %+v", pub.preview.PoolAttachment)
	t.Logf("launch  pool attachment: %+v", pub.launched.PoolAttachment)
	if !reflect.DeepEqual(pub.preview.PoolAttachment, pub.launched.PoolAttachment) {
		t.Errorf("preview/launch pool drift for the same webhook: preview=%+v launch=%+v", pub.preview.PoolAttachment, pub.launched.PoolAttachment)
	}
}

// RVA-E: the elected signal reconciler warms "stored attachments' pools
// PLUS schedule-bound pools" — its own comment calls leaving a
// schedule-only pool out a defect (the margin skip silently inert on that
// lane). A pool reached only through a forge integration's block (the
// webhook config, the enforcement half) is left out the same way.
func TestPoolForgeReview_SignalSweepMissesAPoolBoundOnlyByTheIntegration(t *testing.T) {
	for _, lane := range []string{"schedule (control)", "forge integration / webhook config"} {
		t.Run(lane, func(t *testing.T) {
			s, _ := newTeamForkServer(t, &tierPublisher{})
			wirePoolFold(t, s)
			ctx := context.Background()
			sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
			if err != nil {
				t.Fatal(err)
			}
			s.sealer = sealer
			oauth := secrets.NewMemoryOAuthStore()
			s.oauthStore = oauth
			sealed, err := secrets.SealOAuthPayload(sealer, "lender-1", secrets.OAuthKindClaudeCode, []byte(`{"claudeAiOauth":{"accessToken":"redacted-test"}}`))
			if err != nil {
				t.Fatal(err)
			}
			if err := oauth.Upsert(ctx, secrets.OAuthRecord{ID: "rec-1", UserID: "lender-1", Kind: secrets.OAuthKindClaudeCode, SealedPayload: sealed, Fingerprint: "fp-1"}); err != nil {
				t.Fatal(err)
			}
			if err := s.credPoolPledges.Upsert(ctx, credpool.Pledge{ID: "pl-1", PoolID: "pool-bound", UserID: "lender-1",
				Credential: credpool.Credential{Source: credpool.SourceOAuth, Ref: string(secrets.OAuthKindClaudeCode)}, Enabled: true}); err != nil {
				t.Fatal(err)
			}
			s.usageCaps = usagecap.NewMemStore()
			probed := 0
			s.poolUsageProbe = func(context.Context, []byte) ([]usagecap.Reading, error) { probed++; return nil, nil }
			sched := cloudsched.NewMemoryStore()
			s.scheduledBots = sched
			bound := &credpool.Binding{PoolID: "pool-bound", Enabled: true}
			if lane == "schedule (control)" {
				if err := sched.Create(ctx, cloudsched.ScheduledBot{ID: "sb-1", TenantID: "t1", BotID: "probe", Cron: "0 2 * * 1", Pool: bound}); err != nil {
					t.Fatal(err)
				}
			} else {
				cs := webhooks.NewMemoryConfigStore()
				s.webhookConfigs = cs
				if err := cs.Create(ctx, webhooks.Config{ID: "wh-1", TenantID: "t1", Enabled: true, Pool: bound, ProvisionedBy: "forge:c1"}); err != nil {
					t.Fatal(err)
				}
				ris := forge.NewMemoryRepoIntegrationStore()
				s.forgeIntegrations = ris
				if err := ris.Create(ctx, forge.RepoIntegration{ID: "ri-1", TenantID: "t1", WebhookID: "wh-1", RepoFullName: "group/api", Pool: bound}); err != nil {
					t.Fatal(err)
				}
			}
			s.poolSignalSyncSweep(ctx)
			t.Logf("%s: probes run by the sweep = %d", lane, probed)
			if probed == 0 {
				t.Errorf("%s: the bound pool's claude_code entry was never warmed — the launch-path margin skip reads a ledger nobody fills", lane)
			}
		})
	}
}

// RVA-F: the pool DELETE refuses while an ATTACHMENT names the pool ("a
// pool an attachment resolves to refuses launches the moment it vanishes")
// but reads no binding block — the integration/webhook config (new here)
// and the schedule rows. The delete lands 200 and every launch of the
// bound integration is refused from then on.
func TestPoolForgeReview_PoolDeleteIgnoresBindingBlocks(t *testing.T) {
	s, _ := poolRoutesServer(t)
	ctx := context.Background()
	if err := s.credPoolPools.Upsert(ctx, credpool.Pool{ID: "pool-o", OrgID: "org1", Enabled: true, OwnerType: credpool.PoolOwnerOrg, OwnerID: "org1"}); err != nil {
		t.Fatal(err)
	}
	bound := &credpool.Binding{PoolID: "pool-o", Enabled: true}
	cs := webhooks.NewMemoryConfigStore()
	s.webhookConfigs = cs
	if err := cs.Create(ctx, webhooks.Config{ID: "wh-1", TenantID: "team1", Enabled: true, Pool: bound, ProvisionedBy: "forge:c1"}); err != nil {
		t.Fatal(err)
	}
	r := poolRouteReq(auth.WithIdentity(ctx, auth.Identity{UserID: "jo"}), http.MethodDelete, "/api/pools/pool-o", "")
	r.SetPathValue("id", "pool-o")
	w := httptest.NewRecorder()
	s.handleDeletePool(w, r)
	t.Logf("DELETE pool named by a webhook binding block: code=%d body=%s", w.Code, w.Body.String())
	_, ferr := s.resolveRunPoolAttachment(ctx, "team1", "review-pr", credpool.BindingLayer{Source: "webhook", Enabled: true, PoolID: "pool-o"})
	t.Logf("the bound integration's next launch fold: %v", ferr)
	if w.Code == http.StatusOK {
		t.Errorf("the pool was deleted while a binding block names it — every launch of that integration now refuses")
	}
}

// RVA-G: an Enabled=false block grants NOTHING (the fold stops there and
// never even reads the pool — route_resolve.go's disabled arm returns
// before HasPool), yet validatePoolBindingWrite demands the named pool
// exist, be ENABLED and CONSENT, and refuses the platform pool outright.
// So "opt this repo out of the inherited (platform-attached) pool" has no
// expressible block when the team owns no other consenting pool.
func TestPoolForgeReview_DisabledBlockIsRefusedAtTheWriteButHonoredAtTheFold(t *testing.T) {
	// The fold honors a disabled binding naming the platform pool: the
	// inherited platform attachment is blocked.
	pub := &tierPublisher{}
	fs, _ := newTeamForkServer(t, pub)
	wirePoolFold(t, fs)
	ctx := context.Background()
	if err := fs.credPoolPools.Upsert(ctx, credpool.Pool{ID: "pool-platform", Enabled: true, OwnerType: credpool.PoolOwnerPlatform}); err != nil {
		t.Fatal(err)
	}
	if err := fs.credPoolAttachments.Put(ctx, credpool.Attachment{ScopeID: credpool.ScopePlatform, PoolID: "pool-platform", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	inherit, err := fs.resolveRunPoolAttachment(ctx, "t1", "probe")
	if err != nil {
		t.Fatal(err)
	}
	optOut, err := fs.resolveRunPoolAttachment(ctx, "t1", "probe", credpool.BindingLayer{Source: "webhook", Enabled: false, PoolID: "pool-platform"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fold, no block: %+v", inherit)
	t.Logf("fold, disabled block naming the platform pool: %+v", optOut)

	// The write surface refuses both opt-out shapes.
	s, connID, done := approvalPoolServer(t)
	defer done()
	if err := s.credPoolPools.Upsert(ctx, credpool.Pool{ID: "pool-platform", Enabled: true, OwnerType: credpool.PoolOwnerPlatform}); err != nil {
		t.Fatal(err)
	}
	off := false
	if _, err := s.credPoolPools.Patch(ctx, "pool-o1", credpool.PoolPatch{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	for name, block := range map[string]string{
		"opt out of the platform pool":           `{"pool_id":"pool-platform","enabled":false}`,
		"opt out naming a now-disabled org pool": `{"pool_id":"pool-o1","enabled":false}`,
	} {
		w := httptest.NewRecorder()
		s.handleEnableForgeRepoBots(w, forgeReq(teamAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots",
			`{"connection_id":"`+connID+`","repo":"group/api","bot_ids":["review-pr"],"pool":`+block+`}`, "t1"))
		t.Logf("%s: code=%d body=%s", name, w.Code, strings.TrimSpace(w.Body.String()))
		if w.Code == http.StatusBadRequest {
			t.Errorf("%s: a block that grants nothing is refused at the write", name)
		}
	}
}

// RVA-C (second card lane): pipeline admission (launchTicketNow), the
// operator's "launch now" and the Ready sweep — same cardLaunchPolicyLayers
// read, same missing pool layer.
func TestPoolForgeReview_PipelineAdmissionDropsTheIntegrationsPoolBlock(t *testing.T) {
	pub := &tierPublisher{}
	s, _ := newTeamForkServer(t, pub)
	wirePoolFold(t, s)
	seedTeamPoolAttachment(t, s)
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
	cfg := webhooks.Config{ID: "wh1", TenantID: "t1", Provider: webhooks.ProviderGitHub, Enabled: true, BotIDs: []string{"probe"},
		Pool: &credpool.Binding{PoolID: "pool-bound", Enabled: false}}
	seedIntegrationConfig(t, s, cfg)
	s.ensureBoardCard(boundedCtx(t), cfg, webhooks.CommandRoute{BotID: "probe", Mode: "board"},
		map[string]string{"pr_url": "https://forge.example/o/r/pull/7"},
		webhookEventMeta{Kind: "note", SubjectID: "note:1"}, native.StateReady, "https://forge.example/o/r.git", "main")
	cards, err := board.List(native.ListFilter{})
	if err != nil || len(cards) != 1 {
		t.Fatalf("cards: %v %d", err, len(cards))
	}
	if _, err := s.launchTicketNow(boundedCtx(t), "t1", systemLaunch(pipelineAdmissionActor), s.runs, board, cards[0]); err != nil {
		t.Fatalf("launchTicketNow = %v", err)
	}
	got := pub.only(t).PoolAttachment
	t.Logf("ticket lane pool attachment under an integration opt-out: %+v", got)
	if got != nil && got.PoolID != "" {
		t.Errorf("the integration's enabled:false block did not block inheritance on the ticket lane: drew %q from %s", got.PoolID, got.ScopeID)
	}
}

// RVA-I (bas): an org-less team (legacy/local rows) named EXPLICITLY in a
// user pool's audience (Audience.Teams / AllTeams) is refused at the
// binding write — the "team resolves no org" guard runs before the
// audience the pool owner wrote — while the attachment consent
// (poolAttachmentConsent) admits it.
func TestPoolForgeReview_OrglessTeamNamedInAUserPoolAudienceIsRefused(t *testing.T) {
	s, _ := poolRoutesServer(t)
	ctx := context.Background()
	if _, err := s.authStore().CreateTeam(ctx, identity.Team{ID: "t-noorg", Name: "t-noorg", Slug: "t-noorg"}); err != nil {
		t.Fatal(err)
	}
	if err := s.credPoolPools.Upsert(ctx, credpool.Pool{ID: "pool-alice", Enabled: true, OwnerType: credpool.PoolOwnerUser, OwnerID: "alice",
		Visibility: credpool.PoolVisibilityAudience, Audience: credpool.Audience{Teams: []string{"t-noorg"}}}); err != nil {
		t.Fatal(err)
	}
	err := s.validatePoolBindingWrite(ctx, "t-noorg", &credpool.Binding{PoolID: "pool-alice", Enabled: true})
	pool, _ := s.credPoolPools.Get(ctx, "pool-alice")
	t.Logf("binding write: %v; audience admits the team: %v", err, pool.Audience.Allows(pool.OrgID, "", "t-noorg", "", false))
	if err != nil {
		t.Errorf("the pool owner's explicit audience entry for this team is overridden by the org-resolution guard")
	}
}
