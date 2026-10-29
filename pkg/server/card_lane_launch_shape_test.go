package server

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The card lanes' launch shape (ADR-129 §7, #2665): a card's vars are the
// delivery payload's answer and no operator gesture follows, so a routing
// field a {{vars.*}} span leaves undecided refuses the launch — where the
// direct lane keeps launching, and a span a declared literal default
// decides never refuses (the measured bound: zero shipped refusals). The
// undecided case is a DECLARED var whose default consults the environment:
// the view marks it late, the span stays written, and the compiler is
// silent — the refusal is the launch shape's own.
const templatedUndecidedBot = "agent TEAMFORK:\n  backend: \"{{vars.lane_backend}}\"\n  model: \"${ITERION_SHAPE_MODEL_TEST:-openai/gpt-5.6-sol}\"\n\nvars:\n  lane_backend: string = \"${ITERION_SHAPE_BACKEND:-claude_code}\"\n\nworkflow team_model:\n  entry: TEAMFORK\n  TEAMFORK -> done\n"

const templatedDefaultedBot = "agent TEAMFORK:\n  backend: \"{{vars.lane_backend}}\"\n  model: \"${ITERION_SHAPE_MODEL_TEST:-openai/gpt-5.6-sol}\"\n\nvars:\n  lane_backend: string = \"claude_code\"\n\nworkflow team_model:\n  entry: TEAMFORK\n  TEAMFORK -> done\n"

func seedShapeBot(t *testing.T, s *Server, source string) {
	t.Helper()
	row, err := s.botSources.GetBySlug(store.WithTenant(t.Context(), "t1"), "t1", "probe")
	if err != nil {
		t.Fatal(err)
	}
	row.Files[botsource.MainBotFile] = source
	if _, err := s.botSources.Update(store.WithTenant(t.Context(), "t1"), row); err != nil {
		t.Fatal(err)
	}
}

// shapeCard seeds one payload-derived card for the probe bot and returns
// the board it sits on.
func shapeCard(t *testing.T, s *Server) native.BoardStore {
	t.Helper()
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	s.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
	integration := webhooks.Config{ID: "wh1", TenantID: "t1", Provider: webhooks.ProviderGitHub}
	seedIntegrationConfig(t, s, integration)
	s.ensureBoardCard(boundedCtx(t), integration,
		webhooks.CommandRoute{BotID: "probe", Mode: "board", ArgsVar: "scope_notes"},
		map[string]string{"scope_notes": "please review", "pr_url": "https://forge.example/o/r/pull/7"},
		webhookEventMeta{Kind: "note", SubjectID: "note:1"}, "", "", "")
	return board
}

// A carded run of a bot whose backend a {{vars.*}} span leaves undecided
// refuses — the payload's answer is the only answer a card gets.
func TestCardLaneRefusesAnUndecidedRoutingField(t *testing.T) {
	pub := &tierPublisher{}
	s, rs := newTeamForkServer(t, pub)
	pub.onLaunch = func(runID string) { finishRunAs(t, rs, runID, store.RunStatusFinished) }
	seedShapeBot(t, s, templatedUndecidedBot)
	board := shapeCard(t, s)
	cards, err := board.List(native.ListFilter{})
	if err != nil || len(cards) != 1 {
		t.Fatalf("board.List = %v, %v; want one card", cards, err)
	}
	if err := s.processBoardCard(boundedCtx(t), "t1", *cards[0]); err == nil {
		t.Fatal("the card lane launched a routing field its vars cannot decide")
	} else if !strings.Contains(err.Error(), "cannot decide") || !strings.Contains(err.Error(), "TEAMFORK") || !strings.Contains(err.Error(), "backend") || !strings.Contains(err.Error(), "lane_backend") {
		t.Fatalf("refusal = %v, want the launch shape's own refusal naming the node, the field and the variable", err)
	}
	// The ticket lane reads the same board: the same shape.
	pub2 := &tierPublisher{}
	s2, rs2 := newTeamForkServer(t, pub2)
	pub2.onLaunch = func(runID string) { finishRunAs(t, rs2, runID, store.RunStatusFinished) }
	seedShapeBot(t, s2, templatedUndecidedBot)
	s2.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
	seedIntegrationConfig(t, s2, webhooks.Config{ID: "wh1", TenantID: "t1", Provider: webhooks.ProviderGitHub})
	if _, err := s2.launchTicketNow(boundedCtx(t), "t1", s2.runs, board, cards[0]); err == nil {
		t.Fatal("the ticket lane launched a routing field its vars cannot decide")
	}
}

// The measured bound: a span the declared literal default decides never
// refuses — the shipped docs-refresh shape stays launchable by card.
func TestCardLaneLaunchesASpanTheDeclaredDefaultDecides(t *testing.T) {
	pub := &tierPublisher{}
	s, rs := newTeamForkServer(t, pub)
	pub.onLaunch = func(runID string) { finishRunAs(t, rs, runID, store.RunStatusFinished) }
	seedShapeBot(t, s, templatedDefaultedBot)
	board := shapeCard(t, s)
	cards, err := board.List(native.ListFilter{})
	if err != nil || len(cards) != 1 {
		t.Fatalf("board.List = %v, %v; want one card", cards, err)
	}
	if err := s.processBoardCard(boundedCtx(t), "t1", *cards[0]); err != nil {
		t.Fatalf("the declared literal default was refused: %v", err)
	}
	pub.only(t) // exactly one launch reached the publisher; only() fatals otherwise
}

// The direct lane keeps its shape: the same undecided bot launches from
// the webhook lane, whose vars the caller and the config layers shape.
func TestDirectLaneKeepsLaunchingTheUndecidedBot(t *testing.T) {
	pub := &tierPublisher{}
	s, rs := newTeamForkServer(t, pub)
	pub.onLaunch = func(runID string) { finishRunAs(t, rs, runID, store.RunStatusFinished) }
	seedShapeBot(t, s, templatedUndecidedBot)
	configs := webhooks.NewMemoryConfigStore()
	s.webhookConfigs = configs
	cfg := webhooks.Config{ID: "hook", TenantID: "t1", Enabled: true, BotIDs: []string{"probe"}, DefaultBotID: "probe"}
	if err := configs.Create(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	ctx := store.WithIdentity(auth.WithIdentity(t.Context(), auth.Identity{UserID: "webhook:hook", TeamID: "t1", Kind: auth.KindWebhook}), "t1", "webhook:hook")
	if _, err := s.launchWebhookBot(ctx, cfg, "probe", map[string]string{"lane_backend": "claw"}, "", "", "", nil, nil, store.RunTrustDefault, "", ""); err != nil {
		t.Fatalf("the direct lane refused what its shape launches: %v", err)
	}
}
