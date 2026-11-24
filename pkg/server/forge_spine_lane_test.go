package server

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/forgetrigger"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/gitlab"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// The lane witnesses of the spine issue cut (ADR-172, plan §2a rows 9, 10 and
// 12): the claims a legacy era left still dedupe a spine-routed redelivery, a
// rollback hands the lane back to legacy without stranding the spine-era
// claims, and two replicas racing one delivery claim it once. They are RED
// first: the handlers have no ForgeRouting branch yet, so a flipped config
// rides the legacy path and fails on the legacy answer.
//
// Unlike the one-shot fixtures of forge_spine_issue_test.go, these witnesses
// flip the config MID-LIFE: a legacy phase runs first, the routing flips (or
// rolls back), and the same delivery comes back. The fixtures here project
// the rows from the CATALOGUE the files parse to, so the rows' projection
// digest is the digest a delivery recomputes — never a stale one.

// spineBotManifest renders the .bot file of one fixture bot: a manifest whose
// kind=forge issue_labeled invocation declares exactly the decl's mode and
// labels, its context_vars when it carries any, and — when the decl pins
// manifest launch vars — the forge: block that carries them. The catalogue
// entry the file parses to and the projected row must carry the same
// invocation, or the router's staleness check would refuse every delivery of
// the fixture.
func spineBotManifest(d spineBotDecl) string {
	var b strings.Builder
	b.WriteString("invocations:\n  - kind: forge\n")
	if d.mode == bundle.ExecutionBoard {
		b.WriteString("    mode: board\n")
	}
	if len(d.context) > 0 {
		b.WriteString("    context_vars:\n")
		for _, k := range slices.Sorted(maps.Keys(d.context)) {
			b.WriteString("      " + k + ": \"" + d.context[k] + "\"\n")
		}
	}
	b.WriteString("    forge:\n      event: issue_labeled\n      labels: [")
	for i, l := range d.labels {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(`"` + l + `"`)
	}
	b.WriteString("]\n")
	if len(d.manifest) > 0 {
		b.WriteString("forge:\n  events: [issue_labeled]\n  webhook:\n    launch_vars:\n")
		for _, k := range slices.Sorted(maps.Keys(d.manifest)) {
			b.WriteString("      " + k + ": \"" + d.manifest[k] + "\"\n")
		}
	}
	return b.String()
}

// spineWriteCatalogueBots writes every fixture bot into the server's
// workspace as a BUNDLE directory (manifest.yaml + main.bot): a loose .bot
// file's entry carries no invocations into the registry, so a fixture built
// on loose files projects zero rows and the router would refuse every
// delivery as unprojected. Run BEFORE the first delivery of any phase: the
// label door's visibility gate reads the catalogue on every launch, and a bot
// missing from it is filtered, not launched.
func spineWriteCatalogueBots(t *testing.T, s *Server, bots ...spineBotDecl) {
	t.Helper()
	for _, b := range bots {
		dir := filepath.Join(s.cfg.WorkDir, "bots", b.bot)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("make the bundle dir of %s: %v", b.bot, err)
		}
		manifest := "name: " + b.bot + "\nschema_version: 1\n" + spineBotManifest(b)
		if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o644); err != nil {
			t.Fatalf("write the manifest of %s: %v", b.bot, err)
		}
		stub := "agent x:\n  model: \"test\"\n"
		if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(stub), 0o644); err != nil {
			t.Fatalf("write main.bot of %s: %v", b.bot, err)
		}
	}
}

// spineFlipConfig stamps what a flip to spine writes: the issues lane routed
// by subscription, SpineSince now (it never moves again), and the integration
// whose binding the rows project under.
func spineFlipConfig(cfg webhooks.Config) webhooks.Config {
	now := time.Now().UTC()
	cfg.ForgeRouting.Issues = webhooks.RoutingSpine
	cfg.SpineSince = &now
	cfg.ProvisionIntegrationID = spineIntegrationID
	return cfg
}

// spineProjectCatalogueRows projects the bots the way a subscriptions sync
// does: it reads the catalogue entries the fixture's files parse to, plans the
// integration over THOSE inputs — so the plan's digest is the digest a
// delivery recomputes from the same catalogue — and replaces the server's
// trigger store with the plan's rows, which it returns.
func spineProjectCatalogueRows(t *testing.T, s *Server, cfg webhooks.Config, bots ...spineBotDecl) []trigger.Subscription {
	t.Helper()
	names := spineBotIDs(bots)
	// The projector plans over the INTEGRATION's whole bot set (ri.BotIDs),
	// bots that claim no issue event included: their manifests feed the
	// digest too. The config's bot scope is that set here.
	scope := slices.Clone(cfg.BotIDs)
	for _, n := range names {
		if !slices.Contains(scope, n) {
			scope = append(scope, n)
		}
	}
	slices.Sort(scope)
	bound := map[string]bool{}
	for _, n := range scope {
		bound[n] = true
	}
	entries, err := s.effectiveEntriesFor(context.Background(), cfg.TenantID)
	if err != nil {
		t.Fatalf("read the catalogue the rows project from: %v", err)
	}
	invByBot := map[string][]bundle.Invocation{}
	frByBot := map[string]*bundle.ForgeRequirements{}
	for _, e := range entries {
		if !bound[e.Name] {
			continue
		}
		invByBot[e.Name] = e.Invocations
		frByBot[e.Name] = e.Forge
	}
	for _, n := range names {
		inv, ok := invByBot[n]
		if !ok {
			t.Fatalf("bot %s is not in the catalogue — its manifest was not written or does not parse", n)
		}
		// Every fixture bot declares a kind=forge issue_labeled invocation;
		// an entry that parsed to none means the fixture wrote a shape the
		// registry does not read (a loose .bot file carries no invocations),
		// and the plan would project zero rows.
		hasForge := false
		for _, i := range inv {
			if i.Kind == bundle.InvocationKindForge {
				hasForge = true
			}
		}
		if !hasForge {
			t.Fatalf("bot %s's catalogue entry carries no kind=forge invocation (%d invocations) — the fixture bundle does not parse", n, len(inv))
		}
	}
	roles := forgetrigger.RoleBots{Implementer: featureDevBotID, Reviewer: defaultWebhookBotReviewPR}
	ri := forge.RepoIntegration{
		ID: spineIntegrationID, TenantID: cfg.TenantID,
		RepoFullName: spineRepo, WebhookID: cfg.ID, BotIDs: scope,
	}
	spineStoreIntegration(t, s, ri)
	plan := forgetrigger.PlanIntegration(forge.SubscriptionInput{
		Integration: ri,
		Config:      cfg, Bots: scope, InvByBot: invByBot, FrByBot: frByBot,
	}, roles, time.Now().UTC())
	if plan.Err != nil {
		t.Fatalf("plan the spine rows: %v", plan.Err)
	}
	st := trigger.NewMemorySubscriptionStore()
	for _, row := range plan.Rows {
		if err := st.Create(context.Background(), row); err != nil {
			t.Fatalf("store row %s of bot %s: %v", row.ID, row.BotID, err)
		}
	}
	s.cfg.TriggerStore = st
	return plan.Rows
}

// spineRollFixture is the world of a config that flips (or rolls back)
// mid-life: the hybrid harness (a coordinator-wired board whose cards are
// claims AND a recording direct launch tail), the config the current phase's
// deliveries carry, and both providers' signed delivery functions.
type spineRollFixture struct {
	s       *Server
	runs    *laneRuns
	board   native.BoardStore
	counter *orgusage.MemoryCounter
	secret  string
	// cfg is what the NEXT delivery carries: narrow it for a legacy phase
	// (the sole bot the election may pick), flip it for the spine phase.
	cfg webhooks.Config
	// body builds the provider's issues payload adding exactly these labels,
	// in payload order.
	body func(added ...string) string
	// deliver posts a body with an optional forge delivery id.
	deliver func(body string, deliveryID ...string) *httptest.ResponseRecorder

	mu       sync.Mutex
	launched []spineIssueLaunch
}

// newSpineRollGitHubFixture builds the GitHub shape: an hmac config on ghw,
// signed deliveries, every bot's manifest in the workspace, and the config in
// the store (the launch tail's retry path reads it there).
func newSpineRollGitHubFixture(t *testing.T, bots ...spineBotDecl) *spineRollFixture {
	t.Helper()
	s, runs, board, counter := newSpineServer(t)
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = spineBotIDs(bots)
	f := &spineRollFixture{s: s, runs: runs, board: board, counter: counter, secret: pt, cfg: cfg,
		body: func(added ...string) string { return ghIssueLabelBody(added[0]) }}
	spineWriteCatalogueBots(t, s, bots...)
	if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
		t.Fatalf("store the webhook config: %v", err)
	}
	f.deliver = func(body string, deliveryID ...string) *httptest.ResponseRecorder {
		r := ghReq(ghCtx(f.cfg), body, prforge.EventHeaderIssues, f.secret)
		for _, id := range deliveryID {
			r.Header.Set("X-GitHub-Delivery", id)
		}
		w := httptest.NewRecorder()
		s.handleGitHubWebhook(w, r)
		return w
	}
	f.recordLaunch()
	return f
}

// newSpineRollGitHubDirectFixture is the DIRECT-only shape (no coordinator,
// no board): every claim is a Delivery row the launch tail takes. The legacy
// era of a direct legacy claim needs it — with a coordinator wired, the
// legacy label route cards instead of launching.
func newSpineRollGitHubDirectFixture(t *testing.T, bots ...spineBotDecl) *spineRollFixture {
	t.Helper()
	s, runs, counter := newDirectLaneServer(t)
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = spineBotIDs(bots)
	f := &spineRollFixture{s: s, runs: runs, counter: counter, secret: pt, cfg: cfg,
		body: func(added ...string) string { return ghIssueLabelBody(added[0]) }}
	spineWriteCatalogueBots(t, s, bots...)
	if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
		t.Fatalf("store the webhook config: %v", err)
	}
	f.deliver = func(body string, deliveryID ...string) *httptest.ResponseRecorder {
		r := ghReq(ghCtx(f.cfg), body, prforge.EventHeaderIssues, f.secret)
		for _, id := range deliveryID {
			r.Header.Set("X-GitHub-Delivery", id)
		}
		w := httptest.NewRecorder()
		s.handleGitHubWebhook(w, r)
		return w
	}
	f.recordLaunch()
	return f
}

// newSpineRollGitLabDirectFixture is the GitLab twin of the direct-only
// shape.
func newSpineRollGitLabDirectFixture(t *testing.T, bots ...spineBotDecl) *spineRollFixture {
	t.Helper()
	s, runs, counter := newDirectLaneServer(t)
	cfg := glConfig()
	cfg.BotIDs = spineBotIDs(bots)
	f := &spineRollFixture{s: s, runs: runs, counter: counter, cfg: cfg,
		body: glIssueAddedLabelsBody}
	spineWriteCatalogueBots(t, s, bots...)
	if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
		t.Fatalf("store the webhook config: %v", err)
	}
	f.deliver = func(body string, deliveryID ...string) *httptest.ResponseRecorder {
		r := glReq(gitlabCtx(f.cfg), body, gitlab.EventHeaderIssue)
		for _, id := range deliveryID {
			r.Header.Set("Idempotency-Key", id)
		}
		w := httptest.NewRecorder()
		s.handleGitLabWebhook(w, r)
		return w
	}
	f.recordLaunch()
	return f
}

// newSpineRollGitLabFixture builds the GitLab shape: a token-mode config on
// w1, deliveries identified by the delivery id header the lane reads.
func newSpineRollGitLabFixture(t *testing.T, bots ...spineBotDecl) *spineRollFixture {
	t.Helper()
	s, runs, board, counter := newSpineServer(t)
	cfg := glConfig()
	cfg.BotIDs = spineBotIDs(bots)
	f := &spineRollFixture{s: s, runs: runs, board: board, counter: counter, cfg: cfg,
		body: glIssueAddedLabelsBody}
	spineWriteCatalogueBots(t, s, bots...)
	if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
		t.Fatalf("store the webhook config: %v", err)
	}
	f.deliver = func(body string, deliveryID ...string) *httptest.ResponseRecorder {
		r := glReq(gitlabCtx(f.cfg), body, gitlab.EventHeaderIssue)
		for _, id := range deliveryID {
			r.Header.Set("Idempotency-Key", id)
		}
		w := httptest.NewRecorder()
		s.handleGitLabWebhook(w, r)
		return w
	}
	f.recordLaunch()
	return f
}

// recordLaunch wraps the fixture's launch tail so every direct launch is
// recorded with the vars it carried.
func (f *spineRollFixture) recordLaunch() {
	base := f.s.webhookLaunchBot
	f.s.webhookLaunchBot = func(ctx context.Context, bot string, vars map[string]string, repoURL, repoRef, pp string, a, b map[string]string) (string, error) {
		f.mu.Lock()
		f.launched = append(f.launched, spineIssueLaunch{bot: bot, vars: vars})
		f.mu.Unlock()
		return base(ctx, bot, vars, repoURL, repoRef, pp, a, b)
	}
}

// launchesByBot counts the direct launches recorded for bot.
func (f *spineRollFixture) launchesByBot(bot string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, l := range f.launched {
		if l.bot == bot {
			n++
		}
	}
	return n
}

// spineFlip applies the flip to the fixture's config and projects the rows
// from the catalogue: the one gesture the witnesses' "then flip" step means.
func (f *spineRollFixture) spineFlip(t *testing.T, bots ...spineBotDecl) {
	t.Helper()
	f.cfg = spineFlipConfig(f.cfg)
	spineProjectCatalogueRows(t, f.s, f.cfg, bots...)
}

// spineClaimRow finds the delivery row the given idempotency key names.
func (f *spineRollFixture) spineClaimRow(t *testing.T, key string) webhooks.Delivery {
	t.Helper()
	rows, err := f.s.webhookDeliveries.ListByWebhook(context.Background(), f.cfg.TenantID, f.cfg.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.IdempotencyKey == key {
			return r
		}
	}
	t.Fatalf("no delivery row carries key %s (rows: %+v)", key, rows)
	return webhooks.Delivery{}
}

// Row 9 (plan table). A claim the LEGACY era made still dedupes and absorbs
// once the integration routes by subscription: the flip must not strand the
// serving claim nor double-start the bots it serves.
//
// RED today: the handlers have no ForgeRouting branch, so every flipped
// delivery rides the legacy path — the fresh-label half answers the legacy
// absorption (the elected winner's lane) where the spine must start the bot
// the label actually claims.
//
// Mutations (router lot): drop the Legacy walk from readTriggerLane's
// families → the redelivery claims free; compute legacyEventBase with the
// spine trigger → the GitLab redelivery (legacy trigger = added[0], spine
// trigger = the smallest label) claims free; restrict cardedFor to owned
// cards → the GitLab board redelivery cards again.
func TestSpineIssueLabel_LegacyClaimStillDedupes(t *testing.T) {
	t.Run("github direct", func(t *testing.T) {
		bots := []spineBotDecl{
			{bot: "nestor", mode: bundle.ExecutionDirect, labels: []string{"nestor", "*-chaos"}},
			{bot: "saboteur", mode: bundle.ExecutionDirect, labels: []string{"saboteur", "*-chaos"}},
		}
		f := newSpineRollGitHubDirectFixture(t, bots...)
		// The legacy era: nestor is the webhook's sole bot, the legacy
		// election launches it under the run-chaos trigger's legacy chain.
		f.cfg.BotIDs = []string{"nestor"}
		e1 := f.body("run-chaos")
		wantLaunched(t, f.deliver(e1, "d-1"), "the legacy era's claim")
		if got := f.launchesByBot("nestor"); got != 1 {
			t.Fatalf("nestor's legacy launches = %d, want 1", got)
		}
		// The flip: both bots projected, the issues lane on the spine.
		f.spineFlip(t, bots...)
		f.cfg.BotIDs = []string{"nestor", "saboteur"}

		// The redelivery of the legacy-claimed event: a duplicate of the
		// legacy claim, whatever the routing says now.
		wantDuplicate(t, f.deliver(e1, "d-1"), "the spine redelivery of the legacy-claimed event", "run-1")
		if got := f.launchesByBot("nestor"); got != 1 {
			t.Fatalf("nestor's launches after the redelivery = %d, want 1", got)
		}

		// A fresh run-chaos label while the legacy claim serves: nestor is
		// absorbed by its own legacy chain, saboteur starts on its own.
		w := f.deliver(f.body("run-chaos"), "d-2")
		if w.Code != http.StatusAccepted {
			t.Fatalf("a fresh label while the legacy claim serves: code=%d body=%s, want 202 with saboteur launched and nestor absorbed by the legacy claim", w.Code, w.Body.String())
		}
		launches := spineLaunches(t, w)
		if len(launches) != 1 || launches[0]["bot"] != "saboteur" {
			t.Fatalf("launches = %v, want exactly saboteur — nestor's lane is served by the legacy claim", launches)
		}
		if got := f.launchesByBot("saboteur"); got != 1 {
			t.Fatalf("saboteur's launches = %d, want 1", got)
		}
		if got := f.launchesByBot("nestor"); got != 1 {
			t.Fatalf("nestor's launches = %d, want 1 — the fresh label was absorbed, not run twice", got)
		}
	})

	t.Run("github board", func(t *testing.T) {
		bots := []spineBotDecl{
			{bot: "nestor", mode: bundle.ExecutionBoard, labels: []string{"nestor", "*-chaos"}},
			{bot: "saboteur", mode: bundle.ExecutionBoard, labels: []string{"saboteur", "*-chaos"}},
		}
		f := newSpineRollGitHubFixture(t, bots...)
		f.cfg.BotIDs = []string{"nestor"}
		e1 := f.body("run-chaos")
		c1 := wantCarded(t, f.deliver(e1, "d-1"), "the legacy era's card", http.StatusAccepted)
		// The flip.
		f.spineFlip(t, bots...)
		f.cfg.BotIDs = []string{"nestor", "saboteur"}

		// The redelivery: answered by the legacy card, no new card.
		if got := wantCarded(t, f.deliver(e1, "d-1"), "the spine redelivery of the legacy-carded event", http.StatusOK); got != c1 {
			t.Fatalf("the redelivery answered by card %q, want the legacy card %s", got, c1)
		}
		// A fresh run-chaos label while the legacy card serves: nestor is
		// absorbed by it, saboteur cards its own.
		w := f.deliver(f.body("run-chaos"), "d-2")
		if w.Code != http.StatusAccepted {
			t.Fatalf("a fresh label while the legacy card serves: code=%d body=%s, want 202 with saboteur carded and nestor absorbed", w.Code, w.Body.String())
		}
		launches := spineLaunches(t, w)
		if len(launches) != 1 || launches[0]["bot"] != "saboteur" {
			t.Fatalf("launches = %v, want exactly saboteur", launches)
		}
		cards := laneCards(t, f.board)
		if len(cards) != 2 {
			t.Fatalf("cards = %d, want 2 — the legacy card and ONE new one for saboteur", len(cards))
		}
		for _, c := range cards {
			if c.ID != c1 && c.Bot != "saboteur" {
				t.Fatalf("the new card %s is assigned to %q, want saboteur", c.ID, c.Bot)
			}
		}
	})

	// The GitLab shape of both stores: the legacy trigger is the payload's
	// FIRST matching label (zeta), the spine trigger is the smallest matching
	// label of the row — so the redelivery's identity is the key legacy used
	// for THIS event, in the Delivery store and on the card's delivery marker
	// alike.
	t.Run("gitlab direct", func(t *testing.T) {
		bots := []spineBotDecl{
			{bot: "nestor", mode: bundle.ExecutionDirect, labels: []string{"nestor"}},
			{bot: "saboteur", mode: bundle.ExecutionDirect, labels: []string{"saboteur", "*-chaos"}},
		}
		f := newSpineRollGitLabDirectFixture(t, bots...)
		f.cfg.BotIDs = []string{"nestor"}
		// Payload order zeta first: the legacy lane keys this event on the
		// zeta trigger, while nestor's projected row claims nestor only.
		e1 := f.body("zeta", "nestor")
		wantLaunched(t, f.deliver(e1, "d-1"), "the legacy era's claim under the payload-order trigger")
		if got := f.launchesByBot("nestor"); got != 1 {
			t.Fatalf("nestor's legacy launches = %d, want 1", got)
		}
		f.spineFlip(t, bots...)
		f.cfg.BotIDs = []string{"nestor", "saboteur"}

		// The redelivery: a duplicate via THIS event's legacy key, not the
		// spine trigger's chain.
		wantDuplicate(t, f.deliver(e1, "d-1"), "the spine redelivery (legacy trigger zeta, spine trigger nestor)", "run-1")
		// A fresh two-label event while the zeta claim serves: the spine
		// trigger is the smallest matching label (nestor), whose lane is
		// free — nestor starts on it; the zeta lane keeps its own claim.
		w := f.deliver(f.body("zeta", "nestor"), "d-2")
		if w.Code != http.StatusAccepted {
			t.Fatalf("a fresh two-label event while the zeta claim serves: code=%d body=%s, want 202 with nestor started on its spine trigger's lane", w.Code, w.Body.String())
		}
		launches := spineLaunches(t, w)
		if len(launches) != 1 || launches[0]["bot"] != "nestor" {
			t.Fatalf("launches = %v, want exactly nestor on the smallest-label lane", launches)
		}
		if got := f.launchesByBot("nestor"); got != 2 {
			t.Fatalf("nestor's launches = %d, want 2 — the spine trigger's lane was free beside the zeta claim", got)
		}
	})

	t.Run("gitlab board", func(t *testing.T) {
		bots := []spineBotDecl{
			{bot: "nestor", mode: bundle.ExecutionBoard, labels: []string{"nestor"}},
			{bot: "saboteur", mode: bundle.ExecutionBoard, labels: []string{"saboteur", "*-chaos"}},
		}
		f := newSpineRollGitLabFixture(t, bots...)
		f.cfg.BotIDs = []string{"nestor"}
		e1 := f.body("zeta", "nestor")
		c1 := wantCarded(t, f.deliver(e1, "d-1"), "the legacy era's card under the payload-order trigger", http.StatusAccepted)
		f.spineFlip(t, bots...)
		f.cfg.BotIDs = []string{"nestor", "saboteur"}

		// The redelivery: the delivery marker legacy stamped on its zeta card
		// answers it, whether or not the reading lane owns the card.
		if got := wantCarded(t, f.deliver(e1, "d-1"), "the spine redelivery of the zeta-carded event", http.StatusOK); got != c1 {
			t.Fatalf("the redelivery answered by card %q, want the legacy zeta card %s", got, c1)
		}
		// A fresh two-label event: nestor's spine trigger's lane (nestor) is
		// free beside the zeta card — a new card for nestor on it.
		w := f.deliver(f.body("zeta", "nestor"), "d-2")
		if w.Code != http.StatusAccepted {
			t.Fatalf("a fresh two-label event while the zeta card serves: code=%d body=%s, want 202 with nestor carded on its spine trigger's lane", w.Code, w.Body.String())
		}
		cards := laneCards(t, f.board)
		if len(cards) != 2 {
			t.Fatalf("cards = %d, want 2 — the zeta card and ONE new one for nestor's spine lane", len(cards))
		}
		for _, c := range cards {
			if c.ID != c1 && c.Bot != "nestor" {
				t.Fatalf("the new card %s is assigned to %q, want nestor", c.ID, c.Bot)
			}
		}
	})
}

// Row 10 (plan table). A rollback hands the lane back to legacy and the
// legacy lane SEES the spine-era claims: a claim the spine made (a direct
// Delivery row, a bot-lane card) still dedupes and absorbs when legacy reads
// the lane again. Run on the coordinator-wired harness (CloudBoardFor +
// CloudBoardCoordinator, the prod shape): legacy then takes cardTriggerLane
// in both halves, as in cloud.
//
// The handlers cannot route by subscription yet, so the spine claim is made
// by driving the launch tail (the exact call the router's steps 7-10 make for
// a direct target, under the very bot-scoped key the router computes) and the
// board store (the exact card a spine board target claims) directly; the
// REDELIVERY goes through the real HTTP handler, which is the surface this
// row pins.
//
// RED today: the legacy handler passes no spine half, so the legacy lane
// reads free beside the spine claim — it cards instead of answering from it,
// in both halves (in cloud the legacy label route always takes the carded
// lane).
//
// Mutations (router lot): the legacy read drops the spine Delivery bases →
// the direct half reds (legacy cards a fresh lane); the legacy board read
// drops the elected bot's lanes from its owned set → the board half reds
// (legacy cards beside the spine card).
func TestLegacyAfterRollback_SeesSpineClaims(t *testing.T) {
	t.Run("spine direct", func(t *testing.T) {
		bots := []spineBotDecl{
			{bot: "nestor", mode: bundle.ExecutionDirect, labels: []string{"nestor"}},
			{bot: "saboteur", mode: bundle.ExecutionBoard, labels: []string{"saboteur"}},
		}
		f := newSpineRollGitHubFixture(t, bots...)
		f.cfg = spineFlipConfig(f.cfg)
		spineProjectCatalogueRows(t, f.s, f.cfg, bots...)

		// The spine era: the router would resolve nestor's target and hand it
		// to the launch tail — this is that call, under the bot-scoped chain
		// the router derives from the trigger's legacy string.
		e1 := f.body("nestor")
		base := botLaneBase(ghBase("nestor"), "nestor")
		req := ghReq(ghCtx(f.cfg), e1, prforge.EventHeaderIssues, f.secret)
		rec := httptest.NewRecorder()
		f.s.acceptWebhookTargets(req.Context(), rec, req, f.cfg, webhookEventMeta{
			Kind: "issues", Action: "labeled", ProjectPath: spineRepo, SubjectID: "issue:42", DeliveryID: "d-1",
		}, []forgeLaunchTarget{{
			BotID: "nestor", IdemKey: base,
			Vars:           map[string]string{"feature_prompt": "from the spine era"},
			subscriptionID: "sub-nestor-1", routedBy: webhooks.RoutingSpine,
		}}, knowledge.ChecksumHex([]byte(e1)), "203.0.113.7")
		if rec.Code != http.StatusAccepted {
			t.Fatalf("the spine claim: code=%d body=%s, want 202 accepted", rec.Code, rec.Body.String())
		}
		if got := f.launchesByBot("nestor"); got != 1 {
			t.Fatalf("nestor's spine launches = %d, want 1", got)
		}
		claim := f.spineClaimRow(t, base)

		// Saboteur leaves the webhook (nestor becomes the sole bot); the lane
		// rolls back to legacy with SpineSince and the rows untouched.
		f.cfg.ForgeRouting.Issues = webhooks.RoutingLegacy
		f.cfg.BotIDs = []string{"nestor"}

		// The redelivery through the real handler: legacy elects nestor, its
		// carded lane reads the spine half and answers from the spine claim —
		// a duplicate of that row, never a fresh card.
		w := f.deliver(e1, "d-1")
		resp := laneResp(t, w)
		if w.Code != http.StatusOK || resp["status"] != "carded" || resp["delivery_id"] != claim.ID {
			t.Fatalf("the rollback redelivery: code=%d body=%s, want 200 carded answered by the spine claim row %s", w.Code, w.Body.String(), claim.ID)
		}
		if n := len(laneCards(t, f.board)); n != 0 {
			t.Fatalf("cards = %d, want 0 — the spine claim answers, legacy cards nothing", n)
		}
		if got := f.launchesByBot("nestor"); got != 1 {
			t.Fatalf("nestor's launches = %d, want 1 — the redelivery is a duplicate, not a run", got)
		}
	})

	t.Run("spine board", func(t *testing.T) {
		bots := []spineBotDecl{
			{bot: "nestor", mode: bundle.ExecutionBoard, labels: []string{"nestor"}},
			{bot: "saboteur", mode: bundle.ExecutionDirect, labels: []string{"saboteur"}},
		}
		f := newSpineRollGitHubFixture(t, bots...)
		f.cfg = spineFlipConfig(f.cfg)
		spineProjectCatalogueRows(t, f.s, f.cfg, bots...)

		// The spine era: the router would card nestor on ITS bot lane — the
		// deterministic lane id, the bot lane label, the event's marker.
		ln := botLane("nestor", "nestor")
		card := seedLaneCard(t, f.board, native.Issue{
			State: native.StateReady, Bot: "nestor",
			Labels:  laneCardLabels(ln, "delivery:d-1"),
			BotArgs: map[string]string{boardProjectPathKey: spineRepo},
		})

		// The rollback: the lane back on legacy, SpineSince untouched, the
		// rows left where they are (inert), nestor the sole bot.
		f.cfg.ForgeRouting.Issues = webhooks.RoutingLegacy
		f.cfg.BotIDs = []string{"nestor"}

		// The redelivery: legacy's carded lane owns the elected bot's bot
		// lanes (the spine half) and answers from the serving card.
		e1 := f.body("nestor")
		w := f.deliver(e1, "d-1")
		resp := laneResp(t, w)
		if w.Code != http.StatusOK || resp["status"] != "carded" || resp["card_id"] != card.ID {
			t.Fatalf("the rollback redelivery: code=%d body=%s, want 200 carded answered by the spine card %s", w.Code, w.Body.String(), card.ID)
		}
		if n := len(laneCards(t, f.board)); n != 1 {
			t.Fatalf("cards = %d, want 1 — the spine card answers, legacy cards nothing", n)
		}
	})
}

// Row 12 (plan table). Two replicas share the Delivery store, the board and
// the trigger store; the same spine-routed delivery hits both concurrently,
// in both arrival orders. Exactly one run for the direct bot and one card for
// the board bot: the claims collide on the lane's unique keys, not on luck.
// A record of the event answers its own bot only, so the replicas may split
// the bots between them — both then answer 202, each for the bot it claimed.
//
// RED today: both replicas run the legacy path and answer the two-label-bot
// 403.
//
// Mutations (router lot): a random card id per delivery → the board bot cards
// twice; a bot-less idempotency key shared by both bots → the direct bots'
// claims collide with each other instead of chaining per bot.
func TestSpineIssueLabel_ReplicaRace(t *testing.T) {
	bots := []spineBotDecl{
		{bot: "nestor", mode: bundle.ExecutionDirect, labels: []string{"nestor", "*-chaos"}},
		{bot: "saboteur", mode: bundle.ExecutionBoard, labels: []string{"saboteur", "*-chaos"}},
	}
	body := ghIssueLabelBody("run-chaos")
	// The race the lane's next key must decide: both replicas read nestor's
	// free chain before either claims it.
	nestorBase := laneGenerationKey(botLaneBase(ghBase("run-chaos"), "nestor"), 0)

	for name, order := range map[string][2]string{
		"in order": {"s1", "s2"},
		"reversed": {"s2", "s1"},
	} {
		t.Run(name, func(t *testing.T) {
			// Shared state: one Delivery store, one board, one trigger store,
			// one run recorder (mutexed — two goroutines feed it).
			deliveries := webhooks.NewMemoryDeliveryStore()
			board, err := native.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			svcs := map[string]*spineRollFixture{}
			replicas := [2]*spineRollFixture{}
			for i, id := range []string{"s1", "s2"} {
				f := newSpineRollGitHubFixture(t, bots...)
				f.s.webhookDeliveries = deliveries
				f.s.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
				f.cfg = spineFlipConfig(f.cfg)
				f.cfg.BotIDs = []string{"nestor", "saboteur"}
				svcs[id] = f
				replicas[i] = f
			}
			// One projection, shared: the rows are the integration's, not a
			// replica's.
			spineProjectCatalogueRows(t, replicas[0].s, replicas[0].cfg, bots...)
			shared := replicas[0].s.cfg.TriggerStore
			integrations := replicas[0].s.forgeIntegrations
			barrier := &barrierDeliveries{DeliveryStore: deliveries, key: nestorBase, armed: 2}
			barrier.arrived.Add(2)
			for _, f := range replicas {
				f.s.webhookDeliveries = barrier
				f.s.cfg.TriggerStore = shared
				f.s.forgeIntegrations = integrations
			}

			var wg sync.WaitGroup
			codes := [2]int{}
			for i, id := range order {
				wg.Add(1)
				go func(i int, id string) {
					defer wg.Done()
					codes[i] = svcs[id].deliver(body, "d-1").Code
				}(i, id)
			}
			wg.Wait()

			// Each bot is claimed by one replica; which replica claims which
			// bot is the race's. A replica that claimed a bot answers 202, one
			// whose every bot was claimed by its twin a 200 duplicate.
			accepted := 0
			for _, code := range codes {
				switch code {
				case http.StatusAccepted:
					accepted++
				case http.StatusOK:
				default:
					t.Fatalf("codes = %v, want each a 202 (claimed a bot) or a 200 duplicate", codes)
				}
			}
			if accepted == 0 {
				t.Fatalf("codes = %v, want at least one 202: some replica claims each bot", codes)
			}
			// One run for the direct bot across BOTH replicas, one card for
			// the board bot.
			if total := replicas[0].launchesByBot("nestor") + replicas[1].launchesByBot("nestor"); total != 1 {
				t.Fatalf("nestor's runs across both replicas = %d, want exactly 1", total)
			}
			cards := laneCards(t, board)
			if len(cards) != 1 || cards[0].Bot != "saboteur" {
				t.Fatalf("cards = %+v, want exactly one saboteur card across both replicas", cards)
			}
		})
	}
}
