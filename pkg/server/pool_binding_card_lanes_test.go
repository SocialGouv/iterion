package server

// Adversarial proof tests (rva round 1, surface reviewer). Written in an
// isolated copy, never in the repository. Each test asserts the PROMISE
// (parity with the direct webhook lane) and is expected to FAIL on HEAD
// 3d0878e19 if the finding is real.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

func rvaDirectLaneAttachment(t *testing.T, cfg webhooks.Config) *store.RunPoolAttachment {
	t.Helper()
	pub := &tierPublisher{}
	s, _ := newTeamForkServer(t, pub)
	wirePoolFold(t, s)
	rvaSeedTeamAttachment(t, s)
	if _, err := s.launchWebhookBot(boundedCtx(t), webhookLaunch(nil, ""), cfg,
		"probe", map[string]string{}, "", "", "acme/repo", nil, nil, store.RunTrustDefault, "", "", ""); err != nil {
		t.Fatalf("launchWebhookBot = %v", err)
	}
	return pub.only(t).PoolAttachment
}

// rvaSeedTeamAttachment: the team t1 attaches pool-bound (a STORED scope),
// so a binding block that says enabled:false is the only thing that keeps
// the pool out of the run.
func rvaSeedTeamAttachment(t *testing.T, s *Server) {
	t.Helper()
	if err := s.credPoolAttachments.Put(context.Background(), credpool.Attachment{
		ScopeID: credpool.TeamScopeID("t1"), PoolID: "pool-bound", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
}

func rvaDescribe(a *store.RunPoolAttachment) string {
	if a == nil {
		return "<nil: no pool>"
	}
	return "pool=" + a.PoolID + " scope=" + a.ScopeID
}

// The board lane (ensureBoardCard -> processBoardCard) for a webhook
// config whose pool block OPTS OUT (enabled:false) while the team attaches
// the pool. The direct lane draws no pool; the card lane must agree.
func TestPoolCardLanes_BoardLaneHonorsTheIntegrationPoolOptOut(t *testing.T) {
	cfg := webhooks.Config{
		ID: "wh1", TenantID: "t1", Provider: webhooks.ProviderGitHub, Enabled: true, BotIDs: []string{"probe"},
		Pool: &credpool.Binding{PoolID: "pool-bound", Enabled: false},
	}
	direct := rvaDirectLaneAttachment(t, cfg)
	t.Logf("direct webhook lane: %s", rvaDescribe(direct))

	pub := &tierPublisher{}
	s, rs := newTeamForkServer(t, pub)
	pub.onLaunch = func(runID string) { finishRunAs(t, rs, runID, store.RunStatusFinished) }
	wirePoolFold(t, s)
	rvaSeedTeamAttachment(t, s)
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
	if err != nil || len(cards) != 1 {
		t.Fatalf("cards = %d (%v)", len(cards), err)
	}
	if err := s.processBoardCard(boundedCtx(t), "t1", *cards[0]); err != nil {
		t.Fatalf("processBoardCard = %v", err)
	}
	card := pub.only(t).PoolAttachment
	t.Logf("board card lane:     %s", rvaDescribe(card))
	if (direct == nil) != (card == nil) || (direct != nil && (direct.PoolID != card.PoolID || direct.ScopeID != card.ScopeID)) {
		t.Fatalf("PARITY BROKEN: the same integration's pool block resolves differently — direct lane %s, board card lane %s", rvaDescribe(direct), rvaDescribe(card))
	}
}

// Same with an ENABLED binding and no stored attachment beyond the team's.
func TestPoolCardLanes_BoardLaneHonorsTheIntegrationPoolBlock(t *testing.T) {
	pub := &tierPublisher{}
	s, rs := newTeamForkServer(t, pub)
	pub.onLaunch = func(runID string) { finishRunAs(t, rs, runID, store.RunStatusFinished) }
	wirePoolFold(t, s)
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
	cfg := webhooks.Config{
		ID: "wh1", TenantID: "t1", Provider: webhooks.ProviderGitHub, Enabled: true, BotIDs: []string{"probe"},
		Pool: &credpool.Binding{PoolID: "pool-bound", Enabled: true},
	}
	seedIntegrationConfig(t, s, cfg)
	s.ensureBoardCard(boundedCtx(t), cfg, webhooks.CommandRoute{BotID: "probe", Mode: "board"},
		map[string]string{"pr_url": "https://forge.example/o/r/pull/7"},
		webhookEventMeta{Kind: "note", SubjectID: "note:1", SubjectURL: "https://forge.example/o/r/pull/7"},
		"", "https://forge.example/o/r.git", "main")
	cards, err := board.List(native.ListFilter{})
	if err != nil || len(cards) != 1 {
		t.Fatalf("cards = %d (%v)", len(cards), err)
	}
	if err := s.processBoardCard(boundedCtx(t), "t1", *cards[0]); err != nil {
		t.Fatalf("processBoardCard = %v", err)
	}
	got := pub.only(t).PoolAttachment
	t.Logf("board card lane: %s", rvaDescribe(got))
	if got == nil || got.PoolID != "pool-bound" || got.ScopeID != "binding:webhook" {
		t.Fatalf("board card lane pool attachment = %s, want pool-bound from binding:webhook (what launchWebhookBot resolves for the same config)", rvaDescribe(got))
	}
}

// The pipeline admission lane (board Ready sweep / operator "launch now")
// for a ticket naming the same integration.
func TestPoolCardLanes_PipelineAdmissionHonorsTheIntegrationPoolBlock(t *testing.T) {
	pub := &tierPublisher{}
	s, _ := newTeamForkServer(t, pub)
	wirePoolFold(t, s)
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := webhooks.Config{
		ID: "wh1", TenantID: "t1", Provider: webhooks.ProviderGitHub, Enabled: true, BotIDs: []string{"probe"},
		Pool: &credpool.Binding{PoolID: "pool-bound", Enabled: true},
	}
	seedIntegrationConfig(t, s, cfg)
	iss, err := board.Create(native.Issue{Title: "ticket", State: native.StateReady, Bot: "probe",
		BotArgs: map[string]string{boardWebhookIDKey: "wh1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.launchTicketNow(boundedCtx(t), "t1", systemLaunch(pipelineAdmissionActor), s.runs, board, iss); err != nil {
		t.Fatalf("launchTicketNow = %v", err)
	}
	got := pub.only(t).PoolAttachment
	t.Logf("pipeline admission lane: %s", rvaDescribe(got))
	if got == nil || got.PoolID != "pool-bound" || got.ScopeID != "binding:webhook" {
		t.Fatalf("pipeline admission pool attachment = %s, want pool-bound from binding:webhook", rvaDescribe(got))
	}
}

// The credential preview with source=webhook, against the launch it claims
// to mirror ("The pool attachment resolves exactly as the launch site does").
func TestPoolCardLanes_WebhookPreviewMatchesTheLaunchPoolAttachment(t *testing.T) {
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
	var out runview.CredentialPreview
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	ctx := store.WithIdentity(auth.WithIdentity(t.Context(), auth.Identity{UserID: "webhook:hook", TeamID: "t1", Kind: auth.KindWebhook}), "t1", "webhook:hook")
	if _, err := s.launchWebhookBot(ctx, webhookLaunch(nil, ""), cfg, "probe", applyWebhookVarLayers(map[string]string{}, cfg, store.RunTrustDefault), "", "", "", nil, nil, store.RunTrustDefault, "", "", ""); err != nil {
		t.Fatal(err)
	}
	t.Logf("preview spec: %s ; launch spec: %s", rvaDescribe(pub.preview.PoolAttachment), rvaDescribe(pub.launched.PoolAttachment))
	if rvaDescribe(pub.preview.PoolAttachment) != rvaDescribe(pub.launched.PoolAttachment) {
		t.Fatalf("PREVIEW PARITY BROKEN: preview resolves %s, the webhook launch resolves %s", rvaDescribe(pub.preview.PoolAttachment), rvaDescribe(pub.launched.PoolAttachment))
	}
}
