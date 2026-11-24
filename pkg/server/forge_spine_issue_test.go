package server

import (
	"context"
	"encoding/json"
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
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/gitlab"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// The spine issue-label witnesses (ADR-172, plan §2a tests 1-6). Every one is
// written against the routed behaviour and is RED first: the handlers have no
// ForgeRouting branch yet, so a spine-routed config rides the legacy path and
// fails on the legacy answer — the two-bot cases on the 403
// `bot "review-pr" not permitted`, the mode/vars cases on the board default
// and the absent context vars. The router lot turns them green.
//
// The fixtures here are named helpers the router lot reuses: the spine-shaped
// config, the rows the projector wrote, and the hybrid harness (a
// coordinator-wired board AND a recording direct launch tail) that lets one
// integration carry a direct bot and a board bot side by side.

const (
	// spineIntegrationID is the integration the witness configs are bound to;
	// the rows the fixture seeds carry integration:<id> as their binding.
	spineIntegrationID = "int-1"
	// spineRepo is the repository every witness payload names (the project
	// path of ghLabeledIssue and glLabeledIssue).
	spineRepo = "acme/widgets"
)

// spineBotDecl is one bot of a spine fixture: the labels its kind=forge
// issue_labeled invocation claims, its execution mode, the invocation's
// context_vars and the bot's own manifest launch vars
// (forge.webhook.launch_vars — per bot, never the config's union).
type spineBotDecl struct {
	bot      string
	mode     bundle.ExecutionMode
	labels   []string
	context  map[string]string
	manifest map[string]string
}

// spineIssueLaunch is one direct launch the fixture recorded.
type spineIssueLaunch struct {
	bot  string
	vars map[string]string
}

// spineIssueFixture is the world one spine witness drives: the hybrid server, the
// seeded rows, and the provider's issues payload builder and delivery
// function.
type spineIssueFixture struct {
	s       *Server
	runs    *laneRuns
	board   native.BoardStore
	counter *orgusage.MemoryCounter
	cfg     webhooks.Config
	// body builds the provider's issues payload adding exactly these labels,
	// in payload order (GitHub carries one per event, GitLab a change set).
	body func(added ...string) string
	// deliver posts the body to the provider's webhook endpoint, with an
	// optional forge delivery id.
	deliver func(body string, deliveryID ...string) *httptest.ResponseRecorder

	mu       sync.Mutex
	launched []spineIssueLaunch
}

// newSpineServer is the hybrid harness: newBoardLaneServer's coordinator
// shape (the card is the launch) with newDirectLaneServer's recording launch
// tail, so one integration can carry a direct bot and a board bot at once —
// witness 1's shape, and the prod shape of a spine integration.
func newSpineServer(t *testing.T) (*Server, *laneRuns, native.BoardStore, *orgusage.MemoryCounter) {
	t.Helper()
	s, board, counter := newBoardLaneServer(t)
	runs := &laneRuns{live: map[string]bool{}}
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		return runs.launch(), nil
	}
	s.webhookRunIsLive = func(_ context.Context, id string) bool {
		runs.mu.Lock()
		defer runs.mu.Unlock()
		return runs.live[id]
	}
	s.webhookCancelRun = func(id string) error {
		runs.mu.Lock()
		defer runs.mu.Unlock()
		runs.canceled = append(runs.canceled, id)
		runs.live[id] = false
		return nil
	}
	return s, runs, board, counter
}

// spineBotIDs lists the fixture's bots in name order — the integration's bot
// set.
func spineBotIDs(bots []spineBotDecl) []string {
	out := make([]string, 0, len(bots))
	for _, b := range bots {
		out = append(out, b.bot)
	}
	slices.Sort(out)
	return out
}

// seedSpineIntegration projects the bots the way a subscriptions sync does —
// forgetrigger.PlanIntegration over the CATALOGUE ENTRIES the fixture's
// manifest files parse to — and writes the rows into the server's trigger
// store under the integration binding. The plan runs over the parsed entries,
// not over a hand-built invocation list: the rows' projection digest must be
// the digest a delivery recomputes from the same catalogue, or the router
// refuses every delivery with 503 subscriptions_stale. cfg gains what the
// flip sets: the issues lane on the spine, SpineSince now, and the
// integration the binding names. The catalogue gains every bot, so the
// visibility gate admits them once the router routes.
func seedSpineIntegration(t *testing.T, s *Server, cfg webhooks.Config, bots ...spineBotDecl) webhooks.Config {
	t.Helper()
	spineWriteCatalogueBots(t, s, bots...)
	names := spineBotIDs(bots)
	bound := map[string]bool{}
	for _, n := range names {
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
		if _, ok := invByBot[n]; !ok {
			t.Fatalf("bot %s is not in the catalogue — its manifest was not written or does not parse", n)
		}
	}
	roles := forgetrigger.RoleBots{Implementer: featureDevBotID, Reviewer: defaultWebhookBotReviewPR}
	ri := forge.RepoIntegration{
		ID:           spineIntegrationID,
		TenantID:     cfg.TenantID,
		RepoFullName: spineRepo,
		WebhookID:    cfg.ID,
		BotIDs:       names,
	}
	spineStoreIntegration(t, s, ri)
	plan := forgetrigger.PlanIntegration(forge.SubscriptionInput{
		Integration: ri,
		Config:      cfg,
		Bots:        names,
		InvByBot:    invByBot,
		FrByBot:     frByBot,
	}, roles, time.Now().UTC())
	if plan.Err != nil {
		t.Fatalf("plan the spine fixture's rows: %v", plan.Err)
	}
	st := trigger.NewMemorySubscriptionStore()
	for _, row := range plan.Rows {
		if err := st.Create(context.Background(), row); err != nil {
			t.Fatalf("store row %s of bot %s: %v", row.ID, row.BotID, err)
		}
	}
	s.cfg.TriggerStore = st
	now := time.Now().UTC()
	cfg.ForgeRouting.Issues = webhooks.RoutingSpine
	cfg.SpineSince = &now
	cfg.ProvisionIntegrationID = spineIntegrationID
	return cfg
}

// spineStoreIntegration writes ri into the server's integration store — the
// record the router reads the digest's bot set from — creating a memory store
// when none is wired, and replacing a record of the same id.
func spineStoreIntegration(t *testing.T, s *Server, ri forge.RepoIntegration) {
	t.Helper()
	if s.forgeIntegrations == nil {
		s.forgeIntegrations = forge.NewMemoryRepoIntegrationStore()
	}
	ctx := context.Background()
	if cur, err := s.forgeIntegrations.Get(ctx, ri.ID); err == nil {
		ri.Version = cur.Version
		if err := s.forgeIntegrations.Update(ctx, ri); err != nil {
			t.Fatalf("update integration %s: %v", ri.ID, err)
		}
		return
	}
	if err := s.forgeIntegrations.Create(ctx, ri); err != nil {
		t.Fatalf("store integration %s: %v", ri.ID, err)
	}
}

// ghIssueLabelBody is the GitHub issues payload applying one label.
func ghIssueLabelBody(label string) string {
	return strings.Replace(ghLabeledIssue, `"label": {"name": "implement"}`, `"label": {"name": "`+label+`"}`, 1)
}

// glIssueAddedLabelsBody is the GitLab issue hook adding every label of
// added, in payload order.
func glIssueAddedLabelsBody(added ...string) string {
	items := make([]string, len(added))
	for i, l := range added {
		items[i] = `{"title": "` + l + `"}`
	}
	list := "[" + strings.Join(items, ", ") + "]"
	body := strings.Replace(glLabeledIssue, `"labels": [{"title": "implement"}]`, `"labels": `+list, 1)
	return strings.Replace(body, `"current": [{"title": "implement"}]`, `"current": `+list, 1)
}

// recordLaunch wraps the fixture's launch tail so every direct launch is
// recorded with the vars it carried.
func (f *spineIssueFixture) recordLaunch() {
	base := f.s.webhookLaunchBot
	f.s.webhookLaunchBot = func(ctx context.Context, bot string, vars map[string]string, repoURL, repoRef, pp string, a, b map[string]string) (string, error) {
		f.mu.Lock()
		f.launched = append(f.launched, spineIssueLaunch{bot: bot, vars: vars})
		f.mu.Unlock()
		return base(ctx, bot, vars, repoURL, repoRef, pp, a, b)
	}
}

// launchesByBot returns the direct launches the fixture recorded for bot.
func (f *spineIssueFixture) launchesByBot(t *testing.T, bot string) []spineIssueLaunch {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []spineIssueLaunch
	for _, l := range f.launched {
		if l.bot == bot {
			out = append(out, l)
		}
	}
	return out
}

// spineLaunches decodes the launches array of a multi-target 202 body.
func spineLaunches(t *testing.T, w *httptest.ResponseRecorder) []map[string]string {
	t.Helper()
	var resp struct {
		Status   string              `json:"status"`
		Launches []map[string]string `json:"launches"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response %d is not JSON: %s", w.Code, w.Body.String())
	}
	return resp.Launches
}

// newSpineGitHubFixture builds the GitHub shape of a spine witness: the
// spine-routed config, its projected rows, and a signed delivery function.
func newSpineGitHubFixture(t *testing.T, bots ...spineBotDecl) *spineIssueFixture {
	t.Helper()
	s, runs, board, counter := newSpineServer(t)
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = spineBotIDs(bots)
	f := &spineIssueFixture{s: s, runs: runs, board: board, counter: counter,
		body: func(added ...string) string { return ghIssueLabelBody(added[0]) }}
	f.cfg = seedSpineIntegration(t, s, cfg, bots...)
	f.deliver = func(body string, deliveryID ...string) *httptest.ResponseRecorder {
		r := ghReq(ghCtx(f.cfg), body, prforge.EventHeaderIssues, pt)
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

// newSpineGitLabFixture builds the GitLab shape of a spine witness.
func newSpineGitLabFixture(t *testing.T, bots ...spineBotDecl) *spineIssueFixture {
	t.Helper()
	s, runs, board, counter := newSpineServer(t)
	cfg := glConfig()
	cfg.BotIDs = spineBotIDs(bots)
	f := &spineIssueFixture{s: s, runs: runs, board: board, counter: counter, body: glIssueAddedLabelsBody}
	f.cfg = seedSpineIntegration(t, s, cfg, bots...)
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

// driveSpineCard launches one card through the real board dispatcher, with
// the integration layers the card names wired on the server. It returns the
// launch spec the publisher captured — the card half of the vars witnesses,
// driven through processBoardCard, never through a helper's own layering.
func driveSpineCard(t *testing.T, botID string, botArgs map[string]string, cfg webhooks.Config) runview.LaunchSpec {
	t.Helper()
	s := newOrgTestServer(t)
	seedBoardLaunchTenant(t, s, "t1")
	botsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(botsDir, botID+".bot"), []byte(testBotMain), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{botsDir}
	s.webhookConfigs = webhooks.NewMemoryConfigStore()
	if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Store = rs
	pub := &tierPublisher{}
	s.runs = newTestRunviewService(t, "", runview.WithStore(rs), runview.WithLaunchPublisher(pub))
	pub.onLaunch = func(runID string) { finishRunAs(t, rs, runID, store.RunStatusFinished) }
	card := native.Issue{ID: "card-" + botID, Bot: botID, State: native.StateReady, BotArgs: botArgs}
	if err := s.processBoardCard(boundedCtx(t), "t1", card); err != nil {
		t.Fatalf("processBoardCard = %v", err)
	}
	return pub.only(t)
}

// spineDeliveryRows lists the delivery audit rows the fixture's webhook
// holds.
func (f *spineIssueFixture) spineDeliveryRows(t *testing.T) []webhooks.Delivery {
	t.Helper()
	rows, err := f.s.webhookDeliveries.ListByWebhook(context.Background(), f.cfg.TenantID, f.cfg.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// wantSpineClaimRow finds the audit row of bot and asserts what the spine
// attribution the tail stamps on it: the subscription that claimed the event.
func wantSpineClaimRow(t *testing.T, rows []webhooks.Delivery, bot string) webhooks.Delivery {
	t.Helper()
	for _, r := range rows {
		if r.BotID != bot {
			continue
		}
		if r.SubscriptionID == "" {
			t.Fatalf("the claim row of %s carries no subscription_id: %+v", bot, r)
		}
		return r
	}
	t.Fatalf("no delivery row for bot %s (rows: %+v)", bot, rows)
	return webhooks.Delivery{}
}

// witness 1 (plan table row 1). RED on main: the legacy election reaches its
// reviewer fallback and answers 403 `bot "review-pr" not permitted` — two
// label bots on one repository start neither.
func TestSpineIssueLabel_TwoBotsEachStartOnTheirOwnLabel(t *testing.T) {
	f := newSpineGitHubFixture(t,
		spineBotDecl{bot: "nestor", mode: bundle.ExecutionDirect, labels: []string{"nestor"}},
		spineBotDecl{bot: "saboteur", mode: bundle.ExecutionBoard, labels: []string{"saboteur"}},
	)
	// Each bot starts on ITS OWN label — one delivery per label (plan row 1;
	// GitHub's labeled action carries exactly one label). One label claimed
	// by BOTH bots is row 3's separate witness.
	w := f.deliver(f.body("nestor"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("the nestor label: code=%d body=%s, want 202 accepted with nestor's run", w.Code, w.Body.String())
	}
	launches := spineLaunches(t, w)
	if len(launches) != 1 || launches[0]["bot"] != "nestor" {
		t.Fatalf("launches = %v, want exactly nestor", launches)
	}
	// nestor started a run through the direct tail.
	if got := f.launchesByBot(t, "nestor"); len(got) != 1 {
		t.Fatalf("nestor's direct launches = %d, want 1", len(got))
	}
	// saboteur's own label: the board half — the card is the launch.
	w = f.deliver(f.body("saboteur"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("the saboteur label: code=%d body=%s, want 202 accepted with saboteur's card", w.Code, w.Body.String())
	}
	launches = spineLaunches(t, w)
	if len(launches) != 1 || launches[0]["bot"] != "saboteur" {
		t.Fatalf("launches = %v, want exactly saboteur", launches)
	}
	// saboteur started a card on the board, assigned to saboteur.
	cards := laneCards(t, f.board)
	if len(cards) != 1 || cards[0].Bot != "saboteur" {
		t.Fatalf("cards = %+v, want one card assigned to saboteur", cards)
	}
	// Every bot leaves its own delivery row carrying the subscription that
	// claimed the event.
	rows := f.spineDeliveryRows(t)
	wantSpineClaimRow(t, rows, "nestor")
	carded := false
	for _, r := range rows {
		if r.BotID == "saboteur" && r.Status == webhooks.StatusCarded {
			if r.CardID == "" || r.SubscriptionID == "" {
				t.Fatalf("saboteur's carded row carries no card_id/subscription_id: %+v", r)
			}
			carded = true
		}
	}
	if !carded {
		t.Fatalf("saboteur left no carded audit row (rows: %+v)", rows)
	}
}

// witness 2, first half (plan table row 2) on GitLab: the same two bots, one
// direct one board, each starting on its own label. RED on main: the legacy
// election answers 403.
func TestSpineIssueLabel_TwoBotsEachStartOnTheirOwnLabel_GitLab(t *testing.T) {
	f := newSpineGitLabFixture(t,
		spineBotDecl{bot: "nestor", mode: bundle.ExecutionDirect, labels: []string{"nestor"}},
		spineBotDecl{bot: "saboteur", mode: bundle.ExecutionBoard, labels: []string{"saboteur"}},
	)
	// Each bot starts on ITS OWN label — one delivery per label, the GitLab
	// twin of the GitHub half above (plan row 1). The one-event-two-labels
	// shape is the sibling TwoAddedLabels witness.
	w := f.deliver(f.body("nestor"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("the nestor label: code=%d body=%s, want 202 accepted with nestor's run", w.Code, w.Body.String())
	}
	launches := spineLaunches(t, w)
	if len(launches) != 1 || launches[0]["bot"] != "nestor" {
		t.Fatalf("launches = %v, want exactly nestor", launches)
	}
	if got := f.launchesByBot(t, "nestor"); len(got) != 1 {
		t.Fatalf("nestor's direct launches = %d, want 1", len(got))
	}
	// saboteur's own label: the board half.
	w = f.deliver(f.body("saboteur"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("the saboteur label: code=%d body=%s, want 202 accepted with saboteur's card", w.Code, w.Body.String())
	}
	launches = spineLaunches(t, w)
	if len(launches) != 1 || launches[0]["bot"] != "saboteur" {
		t.Fatalf("launches = %v, want exactly saboteur", launches)
	}
	cards := laneCards(t, f.board)
	if len(cards) != 1 || cards[0].Bot != "saboteur" {
		t.Fatalf("cards = %+v, want one card assigned to saboteur", cards)
	}
	rows := f.spineDeliveryRows(t)
	wantSpineClaimRow(t, rows, "nestor")
}

// witness 2, second half (plan table row 2): one GitLab event adding TWO
// labels one bot claims launches that bot once, whatever the payload order —
// the spine trigger is the smallest matching label, never
// FirstMatchingLabel's payload-order pick. RED on main: the legacy path
// keeps one label, then answers 403.
func TestSpineGitLabIssue_TwoAddedLabels_OneLaunchPerBot(t *testing.T) {
	bots := []spineBotDecl{
		{bot: "nestor", mode: bundle.ExecutionDirect, labels: []string{"nestor"}},
		{bot: "saboteur", mode: bundle.ExecutionDirect, labels: []string{"saboteur", "*-chaos"}},
	}
	for name, order := range map[string][]string{
		"saboteur first": {"saboteur", "chaos-flag"},
		"chaos first":    {"chaos-flag", "saboteur"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSpineGitLabFixture(t, bots...)
			w := f.deliver(f.body(order...))
			if w.Code != http.StatusAccepted {
				t.Fatalf("two added labels one bot claims: code=%d body=%s, want 202 with one launch for saboteur", w.Code, w.Body.String())
			}
			launches := spineLaunches(t, w)
			if len(launches) != 1 || launches[0]["bot"] != "saboteur" {
				t.Fatalf("launches = %v, want exactly saboteur once", launches)
			}
			if got := f.launchesByBot(t, "saboteur"); len(got) != 1 {
				t.Fatalf("saboteur's launches = %d, want 1 — one bot launches once per event", len(got))
			}
			if got := f.launchesByBot(t, "nestor"); len(got) != 0 {
				t.Fatalf("nestor's launches = %d, want 0 — the event claims saboteur's labels only", len(got))
			}
		})
	}
}

// witness 3 (plan table row 3): one label both bots claim starts both, in
// bot order. RED on main: 403.
func TestSpineIssueLabel_OneLabelClaimedByBothBots_StartsBoth(t *testing.T) {
	f := newSpineGitHubFixture(t,
		spineBotDecl{bot: "nestor", mode: bundle.ExecutionDirect, labels: []string{"nestor", "*-chaos"}},
		spineBotDecl{bot: "saboteur", mode: bundle.ExecutionDirect, labels: []string{"saboteur", "*-chaos"}},
	)
	w := f.deliver(f.body("run-chaos"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("a label both bots claim: code=%d body=%s, want 202 accepted with launches[2]", w.Code, w.Body.String())
	}
	launches := spineLaunches(t, w)
	if len(launches) != 2 {
		t.Fatalf("launches = %v, want one per bot", launches)
	}
	if launches[0]["bot"] != "nestor" || launches[1]["bot"] != "saboteur" {
		t.Fatalf("launches = %v, want [nestor, saboteur] — sorted by bot", launches)
	}
	if got := f.launchesByBot(t, "nestor"); len(got) != 1 {
		t.Fatalf("nestor's launches = %d, want 1", len(got))
	}
	if got := f.launchesByBot(t, "saboteur"); len(got) != 1 {
		t.Fatalf("saboteur's launches = %d, want 1", len(got))
	}
}

// witness 4 (plan table row 4): a bot's own invocation mode and context vars
// reach its run — the direct bot runs direct with force=true in the run vars
// and no card; the board bot's card carries force in its BotArgs. RED on
// main: the legacy route hard-codes board mode and carries no context vars.
func TestSpineIssueLabel_CarriesInvocationModeAndContextVars(t *testing.T) {
	f := newSpineGitHubFixture(t,
		spineBotDecl{bot: "nestor", mode: bundle.ExecutionDirect, labels: []string{"nestor"},
			context: map[string]string{"force": "true"}},
		spineBotDecl{bot: "saboteur", mode: bundle.ExecutionBoard, labels: []string{"saboteur"},
			context: map[string]string{"force": "true"}},
	)
	// Direct: the run vars carry the invocation's context var, and no card.
	w := f.deliver(f.body("nestor"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("the direct bot: code=%d body=%s, want 202 accepted", w.Code, w.Body.String())
	}
	got := f.launchesByBot(t, "nestor")
	if len(got) != 1 || got[0].vars["force"] != "true" {
		t.Fatalf("nestor's run vars = %+v, want force=true carried from the invocation's context_vars", got)
	}
	if n := len(laneCards(t, f.board)); n != 0 {
		t.Fatalf("cards = %d, want none — a direct bot is never carded", n)
	}
	// Board: the card's BotArgs carry the context var, so the dispatcher's
	// launch (which reads BotArgs alone) runs with it.
	w = f.deliver(f.body("saboteur"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("the board bot: code=%d body=%s, want 202 accepted", w.Code, w.Body.String())
	}
	cards := laneCards(t, f.board)
	if len(cards) != 1 || cards[0].Bot != "saboteur" {
		t.Fatalf("cards = %+v, want saboteur's card", cards)
	}
	if cards[0].BotArgs["force"] != "true" {
		t.Fatalf("saboteur's card BotArgs = %v, want force=true — the carded run must run with the invocation's context_vars", cards[0].BotArgs)
	}
}

// witness 5 (plan table row 5): manifest vars do not leak across bots. The
// direct half rides the handler; the card half is driven through the real
// processBoardCard (never through a layering helper), and the client write
// half through the pipeline-board task route — the three surfaces the
// promise binds.
//
// Mutations (router lot): ignore lc.OwnVars in applyCardIntegrationVarLayers
// → the card half reds; drop boardOwnVarsKey from the lift's strip switch →
// red on "absent from the run"; drop it from native.ReservedLaunchArgs → red
// on the client write.
func TestSpineIssueLabel_ManifestVarsDoNotLeakAcrossBots(t *testing.T) {
	t.Run("direct", func(t *testing.T) {
		f := newSpineGitHubFixture(t,
			spineBotDecl{bot: "nestor", mode: bundle.ExecutionDirect, labels: []string{"nestor"},
				manifest: map[string]string{"nestor_var": "from-nestor"}},
			spineBotDecl{bot: "saboteur", mode: bundle.ExecutionDirect, labels: []string{"saboteur"},
				manifest: map[string]string{"saboteur_var": "from-saboteur"}},
		)
		// What provisioning writes on the config: the union of both bots'
		// manifest vars. The spine never applies it — each bot launches with
		// its own row's vars.
		f.cfg.LaunchVars = map[string]string{"nestor_var": "from-nestor", "saboteur_var": "from-saboteur"}
		w := f.deliver(f.body("nestor"))
		if w.Code != http.StatusAccepted {
			t.Fatalf("code=%d body=%s, want 202 accepted for nestor", w.Code, w.Body.String())
		}
		got := f.launchesByBot(t, "nestor")
		if len(got) != 1 {
			t.Fatalf("nestor's launches = %d, want 1", len(got))
		}
		if got[0].vars["nestor_var"] != "from-nestor" {
			t.Fatalf("nestor's run vars = %v, want its own manifest var carried", got[0].vars)
		}
		if v, leaked := got[0].vars["saboteur_var"]; leaked {
			t.Fatalf("saboteur's manifest var leaked into nestor's run (saboteur_var=%q, vars %v): a bot's manifest vars never launch another bot", v, got[0].vars)
		}
	})
	t.Run("card", func(t *testing.T) {
		// The card a spine board target stamps for saboteur: the invocation's
		// own vars as bot args, the own-vars marker, and the integration the
		// launch resolves. Nestor's var is nowhere on it.
		spec := driveSpineCard(t, "saboteur", map[string]string{
			"saboteur_var":    "from-saboteur",
			boardOwnVarsKey:   "1",
			boardWebhookIDKey: "wh-sab",
		}, webhooks.Config{
			ID:       "wh-sab",
			TenantID: "t1",
			// The manifest union, as provisioning writes it.
			LaunchVars: map[string]string{"nestor_var": "from-nestor", "saboteur_var": "from-saboteur"},
		})
		if spec.Vars["saboteur_var"] != "from-saboteur" {
			t.Fatalf("the carded run vars = %v, want saboteur's own var carried", spec.Vars)
		}
		if v, leaked := spec.Vars["nestor_var"]; leaked {
			t.Fatalf("nestor's manifest var leaked into saboteur's carded run (nestor_var=%q, vars %v): under the own-vars marker the launch_vars union is skipped", v, spec.Vars)
		}
		if _, own := spec.Vars[boardOwnVarsKey]; own {
			t.Fatalf("__iterion_own_vars reached the run vars (%v): the lift must strip the marker it consumed", spec.Vars)
		}
	})
	t.Run("client write", func(t *testing.T) {
		// A principal writing a card through the client board write cannot
		// set the reserved marker: the write surface drops the reserved
		// launch-arg class, and __iterion_own_vars is one of them.
		env := newPipelineBoardTestEnv(t)
		resp, err := http.Post(env.http.URL+"/api/v1/pipeline-board/tasks", "application/json",
			strings.NewReader(`{"bot":"review","title":"Own vars","bot_args":{"__iterion_own_vars":"1","scope":"api"}}`))
		if err != nil {
			t.Fatalf("POST task: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status = %d, want 201 created", resp.StatusCode)
		}
		var card native.Issue
		if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
			t.Fatal(err)
		}
		if _, set := card.BotArgs[boardOwnVarsKey]; set {
			t.Fatalf("the created card carries __iterion_own_vars (%v): a client card write cannot set the spine's own-vars marker", card.BotArgs)
		}
		if card.BotArgs["scope"] != "api" {
			t.Fatalf("the created card lost the client's own arg: %v", card.BotArgs)
		}
	})
}

// witness 6 (plan table row 6): the operator pin wins in both modes —
// OperatorLaunchVars force=false beats the invocation's context_vars
// force=true, while the rest of the subscription's vars still reach the run.
// Every issue-lane target is trusted (launchProvenance{}), so there is no
// untrusted half: that witness is P4's, on the fork lane.
//
// RED on main on its own assertion: the spine never carries the
// subscription's vars, so the layered var proving the layering ran is
// absent. Mutation (router lot): layer sub.Vars AFTER cfg.OperatorLaunchVars
// in spineIssueVars — force must then read "true" and the test reds on the
// pin, in both modes.
func TestSpineIssueLabel_OperatorPinWins(t *testing.T) {
	t.Run("direct", func(t *testing.T) {
		f := newSpineGitHubFixture(t,
			spineBotDecl{bot: "nestor", mode: bundle.ExecutionDirect, labels: []string{"nestor"},
				context: map[string]string{"force": "true", "spine_layered": "from-sub"}},
		)
		f.cfg.OperatorLaunchVars = map[string]string{"force": "false"}
		w := f.deliver(f.body("nestor"))
		if w.Code != http.StatusAccepted {
			t.Fatalf("code=%d body=%s, want 202 accepted for nestor", w.Code, w.Body.String())
		}
		got := f.launchesByBot(t, "nestor")
		if len(got) != 1 {
			t.Fatalf("nestor's launches = %d, want 1", len(got))
		}
		if got[0].vars["spine_layered"] != "from-sub" {
			t.Fatalf("nestor's run vars = %v, want the subscription's vars carried under the operator layer", got[0].vars)
		}
		if got[0].vars["force"] != "false" {
			t.Fatalf("force = %q in nestor's run vars (%v), want the operator pin to win", got[0].vars["force"], got[0].vars)
		}
	})
	t.Run("board", func(t *testing.T) {
		// The card the spine stamps carries the invocation's context vars and
		// the own-vars marker; the operator layer applies at launch.
		spec := driveSpineCard(t, "saboteur", map[string]string{
			"force":           "true",
			"spine_layered":   "from-sub",
			boardOwnVarsKey:   "1",
			boardWebhookIDKey: "wh-sab",
		}, webhooks.Config{
			ID:                 "wh-sab",
			TenantID:           "t1",
			OperatorLaunchVars: map[string]string{"force": "false"},
		})
		if spec.Vars["spine_layered"] != "from-sub" {
			t.Fatalf("the carded run vars = %v, want the subscription's vars carried", spec.Vars)
		}
		if spec.Vars["force"] != "false" {
			t.Fatalf("force = %q in the carded run vars (%v), want the operator pin to win", spec.Vars["force"], spec.Vars)
		}
	})
}
