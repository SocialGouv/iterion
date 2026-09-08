package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

func TestBotMetadataReadsTheTierThatServesTheLaunch(t *testing.T) {
	ctx := context.Background()

	// Command discovery routes a /slash-command to a bot AND to the var its
	// args land in. Read from the origin, a fork's command stamps the args
	// under a var the running bundle never declares — an empty prompt, not an
	// error.
	t.Run("command discovery reads the fork's invocation", func(t *testing.T) {
		s := newBotMetadataTeamServer(t)
		route, ok := s.cmdDiscoveryFor(ctx, "t1").LookupCommand("revi")
		if !ok {
			t.Fatal("/revi did not resolve for the forking team")
		}
		if route.ArgsVar != "fork_prompt" {
			t.Errorf("args var = %q, want the FORK's %q — the fork is the bundle that runs", route.ArgsVar, "fork_prompt")
		}
		if !route.OpensMR {
			t.Error("opens_mr = false, want the FORK's true")
		}
		// A command only the fork declares must resolve for that team.
		if _, ok := s.cmdDiscoveryFor(ctx, "t1").LookupCommand("revifork"); !ok {
			t.Error("/revifork did not resolve — the fork declares it, and the fork is what launches")
		}
		// A bot only the team authored is reachable by its own command.
		if r, ok := s.cmdDiscoveryFor(ctx, "t1").LookupCommand("teamonly"); !ok || r.BotID != "teamonly-bot" {
			t.Errorf("/teamonly resolved to %+v (ok=%v), want the team's own bot", r, ok)
		}
		// Another team keeps the baked answer.
		r2, ok := s.cmdDiscoveryFor(ctx, "t2").LookupCommand("revi")
		if !ok || r2.ArgsVar != "scope_notes" || r2.OpensMR {
			t.Errorf("a team with no fork got %+v (ok=%v), want the baked invocation", r2, ok)
		}
		if _, ok := s.cmdDiscoveryFor(ctx, "t2").LookupCommand("revifork"); ok {
			t.Error("/revifork resolved for a team that has no fork — the team tier must not leak across tenants")
		}
	})

	// The labeled-issue lane derives its route from the same invocation.
	t.Run("board route for a label reads the fork's invocation", func(t *testing.T) {
		s := newBotMetadataTeamServer(t)
		route := s.boardRouteForLabel(ctx, "t1", "reviewer-bot")
		if route.ArgsVar != "fork_prompt" {
			t.Errorf("args var = %q, want the FORK's %q", route.ArgsVar, "fork_prompt")
		}
		if !route.OpensMR {
			t.Error("opens_mr = false, want the FORK's true")
		}
	})

	// The converse gate refuses to route to a bot it cannot see. A team's own
	// bot launches (the team tier resolves it) but does not exist here.
	t.Run("the existence probe sees the team's own bot", func(t *testing.T) {
		s := newBotMetadataTeamServer(t)
		cfg := webhooks.Config{ID: "wh-1", TenantID: "t1", WildcardBots: true}
		if !s.canRouteToConverseBot(ctx, cfg, "teamonly-bot") {
			t.Error("a team-authored bot is unroutable — the launch resolves it, so the gate must see it")
		}
		other := webhooks.Config{ID: "wh-2", TenantID: "t2", WildcardBots: true}
		if s.canRouteToConverseBot(ctx, other, "teamonly-bot") {
			t.Error("another team's bot must stay invisible")
		}
	})

	// The config-share mint derives the grant from the bot's declared surface.
	// Read from the origin, it pins paths into a file the fork does not use.
	t.Run("the config-share surface comes from the fork", func(t *testing.T) {
		s := newBotMetadataTeamServer(t)
		spec := s.botConfigShareSpec(ctx, "t1", "reviewer-bot")
		if spec == nil || spec.ConfigPath != "fork.yaml" {
			t.Fatalf("config share = %+v, want the FORK's fork.yaml", spec)
		}
		if base := s.botConfigShareSpec(ctx, "t2", "reviewer-bot"); base == nil || base.ConfigPath != "baked.yaml" {
			t.Fatalf("config share = %+v, want the baked baked.yaml for a team with no fork", base)
		}
	})

	// The forge orchestrator's lookups build the provisioned CommandMap and the
	// event subscription. A team bot the catalog never had makes them fail
	// outright; a fork makes them describe the origin.
	t.Run("the forge orchestrator lookups read the fork", func(t *testing.T) {
		s := newBotMetadataTeamServer(t)
		invs, err := s.forgeBotInvocations(ctx, "t1", "reviewer-bot")
		if err != nil {
			t.Fatalf("invocations: %v", err)
		}
		if len(invs) == 0 || invs[0].ArgsVar != "fork_prompt" {
			t.Errorf("invocations = %+v, want the FORK's args var", invs)
		}
		if _, err := s.forgeBotInvocations(ctx, "t1", "teamonly-bot"); err != nil {
			t.Errorf("a team-authored bot cannot be provisioned: %v", err)
		}
		fr, err := s.forgeBotForge(ctx, "t1", "reviewer-bot")
		if err != nil {
			t.Fatalf("forge requirements: %v", err)
		}
		if fr == nil || len(fr.Events) != 1 || fr.Events[0] != "pull_request_comment" {
			t.Errorf("forge requirements = %+v, want the FORK's `pull_request_comment` event", fr)
		}
	})
}

// The hand-off PRODUCER side asks "where did THIS run write its review". The
// run records the tier that served it (Run.BotSourceTenant), so a fork that
// moves the node its `produces:` names is readable — and a team bot the catalog
// never had produces at all.
func TestHandoffProducerReadsTheRunsOwnTier(t *testing.T) {
	ctx := context.Background()
	// One server and one run store for the whole table: a runview service costs
	// seconds to stop. Each case gets its own PR url, which is the scan's own
	// filter, so the runs cannot see each other.
	s := newBotMetadataTeamServer(t)
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Store = rs
	s.runs = newTestRunviewService(t, "", runview.WithStore(rs))

	seedRun := func(t *testing.T, prURL, botID, tenant, node string) {
		t.Helper()
		tctx := store.WithTenant(ctx, "t1")
		run, err := rs.CreateRun(tctx, mustRunID(t), botID, map[string]any{"pr_url": prURL})
		if err != nil {
			t.Fatal(err)
		}
		run.Status = store.RunStatusFinished
		run.BotID = botID
		run.BotSourceTenant = tenant
		run.BotSourceTier = store.BotSourceTierTeam
		if tenant == "" {
			run.BotSourceTier = store.BotSourceTierBaked
		}
		if err := rs.SaveRun(tctx, run); err != nil {
			t.Fatal(err)
		}
		if err := rs.WriteArtifact(tctx, &store.Artifact{RunID: run.ID, NodeID: node, Data: map[string]any{
			"total_findings": 1,
			"findings": []any{map[string]any{
				"severity": "high", "category": "correctness", "title": "off-by-one",
				"file": "a.go", "line": float64(3),
			}},
		}}); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name, prURL, botID, tenant, node, why string
	}{
		{
			name:  "a fork's own produces: node is read",
			prURL: "https://forge.example/acme/widgets/-/merge_requests/1", botID: "reviewer-bot",
			tenant: "t1", node: "publish_fork",
			why: "the origin's produces: node was read instead of the fork's",
		},
		{
			name:  "a team-only bot's review is handed over",
			prURL: "https://forge.example/acme/widgets/-/merge_requests/2", botID: "teamonly-bot",
			tenant: "t1", node: "publish_teamonly",
			why: "a team-authored producer is invisible to the hand-off",
		},
		{
			// The tier a run was SERVED from decides, not the tenant asking.
			name:  "a baked run still reads the baked produces: node",
			prURL: "https://forge.example/acme/widgets/-/merge_requests/3", botID: "reviewer-bot",
			tenant: "", node: "publish_baked",
			why: "a baked run must still be described by the baked manifest",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seedRun(t, c.prURL, c.botID, c.tenant, c.node)
			got := s.realWebhookHandoff(ctx, webhooks.Config{ID: "wh-1", TenantID: "t1"},
				bundle.HandoffKindReview, handoffQuery{PRURL: c.prURL})
			if !strings.Contains(got, "off-by-one") {
				t.Errorf("%s:\n%q", c.why, got)
			}
		})
	}
}

// The bot home's one-click "enable this trigger" derives a Subscription from
// the bot's manifest invocation and launches through the trigger spine, which
// resolves the team tier (#871). Reading the origin's invocation there builds a
// binding the running bundle never declared.
func TestTriggerFromInvocationReadsTheTeamsFork(t *testing.T) {
	s := newBotMetadataTeamServer(t)
	s.cfg.TriggerStore = trigger.NewMemorySubscriptionStore()

	enable := func(t *testing.T, team, bot string, index int) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(triggerFromInvocationReq{Index: index})
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/api/v1/bots/"+bot+"/triggers/from-invocation", strings.NewReader(string(body)))
		r.SetPathValue("name", bot)
		r = r.WithContext(auth.WithIdentity(r.Context(), auth.Identity{UserID: "u1", TeamID: team}))
		w := httptest.NewRecorder()
		s.handleTriggerFromInvocation(w, r)
		return w
	}

	// Invocation 1 is the schedule in BOTH manifests, with different crons.
	// The subscription launches through the trigger spine, which resolves the
	// team tier — so the cron it carries must be the fork's.
	w := enable(t, "t1", "reviewer-bot", 1)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var sub trigger.Subscription
	if err := json.Unmarshal(w.Body.Bytes(), &sub); err != nil {
		t.Fatal(err)
	}
	if sub.Cron != "0 5 * * 3" {
		t.Errorf("cron = %q, want the FORK's %q — the spine launches the fork", sub.Cron, "0 5 * * 3")
	}

	// A bot only the team authored is enable-able from its own bot home.
	if got := enable(t, "t1", "teamonly-bot", 0); got.Code != http.StatusCreated {
		t.Errorf("status = %d — a team-authored bot is invisible to its own bot home: %s", got.Code, got.Body.String())
	}
}

// A team's fork is not editable through the filesystem-catalog routes: the
// write would land on the baked bundle every other tenant shares.
func TestBotMetadataWritesRefuseATeamFork(t *testing.T) {
	s := newBotMetadataTeamServer(t)
	ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "u1", TeamID: "t1"})

	t.Run("the metadata PUT refuses", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPut, "/api/v1/bots/reviewer-bot", strings.NewReader(`{"icon":"X"}`))
		r.SetPathValue("name", "reviewer-bot")
		r.Header.Set("Origin", "")
		r = r.WithContext(ctx)
		w := httptest.NewRecorder()
		s.handleBotsPut(w, r)
		if w.Code != http.StatusConflict {
			t.Errorf("status = %d, want 409 — editing a team fork through the FS catalog would write the baked bundle every tenant shares (body: %s)", w.Code, w.Body.String())
		}
	})

	t.Run("the workspace overlay refuses", func(t *testing.T) {
		s.cfg.WorkDir = t.TempDir()
		enabled := false
		body, _ := json.Marshal(botOverlayRequest{Enabled: &enabled})
		r := httptest.NewRequest(http.MethodPut, "/api/v1/bots/reviewer-bot/overlay", strings.NewReader(string(body)))
		r.SetPathValue("name", "reviewer-bot")
		r = r.WithContext(ctx)
		w := httptest.NewRecorder()
		s.handleBotOverlay(w, r)
		if w.Code != http.StatusConflict {
			t.Errorf("status = %d, want 409 — the workspace overlay is never consulted for a stored bot, so a 200 changes nothing (body: %s)", w.Code, w.Body.String())
		}
	})

	// The launcher accepts `reviewer_bot` for `reviewer-bot`, so a refusal
	// that only matched the canonical spelling would let the same edit
	// through under the other one.
	t.Run("a non-canonical spelling refuses too", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPut, "/api/v1/bots/reviewer_bot", strings.NewReader(`{"icon":"X"}`))
		r.SetPathValue("name", "reviewer_bot")
		r = r.WithContext(ctx)
		w := httptest.NewRecorder()
		s.handleBotsPut(w, r)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "your team's own bot") {
			t.Errorf("status = %d body = %s, want 409 naming the team's own bot", w.Code, w.Body.String())
		}
	})
}

// A PLATFORM override is refused by the same guard, and by the same spelling
// rule: the origin label must not be stricter than the lookup it labels.
func TestBotMetadataWritesRefuseAPlatformOverride(t *testing.T) {
	s := newBotMetadataTeamServer(t)
	if _, err := s.botSources.Create(
		store.WithTenant(context.Background(), botsource.PlatformTenantID),
		botsource.BotSource{
			TenantID: botsource.PlatformTenantID, Slug: "reviewer-bot",
			Files: map[string]string{botsource.MainBotFile: metaBakedBot, "manifest.yaml": metaBakedManifest},
		}); err != nil {
		t.Fatalf("seed the platform override: %v", err)
	}
	s.platformBots = s.newPlatformBotsResolver()
	// No active team: the platform tier is what this caller sees.
	ctx := auth.WithIdentity(context.Background(), auth.Identity{UserID: "u1"})

	for _, name := range []string{"reviewer-bot", "reviewer_bot"} {
		r := httptest.NewRequest(http.MethodPut, "/api/v1/bots/"+name, strings.NewReader(`{"icon":"X"}`))
		r.SetPathValue("name", name)
		r = r.WithContext(ctx)
		w := httptest.NewRecorder()
		s.handleBotsPut(w, r)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "platform override") {
			t.Errorf("PUT %q: status = %d body = %s, want 409 naming the platform override", name, w.Code, w.Body.String())
		}
	}
}

// Compile-time proof that the forge seam carries a tenant: the orchestrator's
// lookups are what the provisioned CommandMap is built from, and Provision
// already knows the team it provisions for.
var _ forge.BotInvocationsLookup = (&Server{}).forgeBotInvocations
var _ forge.BotForgeLookup = (&Server{}).forgeBotForge
