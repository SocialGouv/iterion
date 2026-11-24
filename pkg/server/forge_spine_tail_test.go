package server

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/trigger"

	iterlog "github.com/SocialGouv/iterion/pkg/log"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The webhook-tail witnesses of the spine issue cut (ADR-172, plan §2a rows
// 13-17, 37, 40, 41, 42): what the spine router answers when nothing claims,
// when a bot is not allowed, when the integration never projected, when the
// lane cannot be read, when a claim write fails mid-fan-out; the repo
// pre-gates a spine-routed config keeps; the audit rows a carded target
// leaves and the reads that must never select it; the ambiguous twin rows
// that must not run twice; the launch_vars hand pin a spine config refuses;
// and the staleness gate. They are RED first: the handlers have no
// ForgeRouting branch yet, so a flipped config rides the legacy path.

// spineLogCapture collects the server's warnings — the errtrack-visible
// surface a handler test can read. Mutexed, so a capture feeding assertions
// never races its writer.
type spineLogCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *spineLogCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *spineLogCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// spineCaptureWarnings replaces the fixture server's logger with one that
// captures every warning.
func spineCaptureWarnings(s *Server) *spineLogCapture {
	log := &spineLogCapture{}
	s.logger = iterlog.New(iterlog.LevelWarn, log)
	return log
}

// spineDeliveryRowsByBot returns the audit rows stamped for bot.
func spineDeliveryRowsByBot(t *testing.T, f *spineRollFixture, bot string) []webhooks.Delivery {
	t.Helper()
	var out []webhooks.Delivery
	for _, r := range f.spineAllDeliveryRows(t) {
		if r.BotID == bot {
			out = append(out, r)
		}
	}
	return out
}

// spineAllDeliveryRows lists the fixture webhook's audit rows.
func (f *spineRollFixture) spineAllDeliveryRows(t *testing.T) []webhooks.Delivery {
	t.Helper()
	rows, err := f.s.webhookDeliveries.ListByWebhook(context.Background(), f.cfg.TenantID, f.cfg.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// Row 13, first half (plan table). A label no subscription claims is a 200
// filtered whose reason names every candidate and the labels it wanted —
// never a 4xx: the forge must keep the hook enabled on a routing the operator
// just changed.
//
// RED today: the legacy election reaches its reviewer fallback and answers
// 403 `bot "review-pr" not permitted`.
func TestSpineIssueLabel_NoClaim_Is200FilteredWithReason(t *testing.T) {
	bots := []spineBotDecl{
		{bot: "nestor", mode: bundle.ExecutionDirect, labels: []string{"nestor"}},
		{bot: "saboteur", mode: bundle.ExecutionDirect, labels: []string{"saboteur"}},
	}
	f := newSpineRollGitHubDirectFixture(t, bots...)
	f.spineFlip(t, bots...)
	w := f.deliver(f.body("bug"), "d-1")
	resp := laneResp(t, w)
	if w.Code != http.StatusOK || resp["status"] != webhooks.StatusFiltered {
		t.Fatalf("a label no subscription claims: code=%d body=%s, want 200 filtered with the no-claim reason", w.Code, w.Body.String())
	}
	reason := resp["reason"]
	if !strings.Contains(reason, `nestor wants [nestor]`) || !strings.Contains(reason, `saboteur wants [saboteur]`) {
		t.Fatalf("the no-claim reason = %q, want every candidate named with the labels it wants", reason)
	}
	if got := f.launchesByBot("nestor") + f.launchesByBot("saboteur"); got != 0 {
		t.Fatalf("launches = %d, want 0", got)
	}
	for _, r := range f.spineAllDeliveryRows(t) {
		if r.SubscriptionID != "" {
			t.Fatalf("the no-claim delivery claimed %s (%+v): nothing was claimed", r.BotID, r)
		}
	}
}

// Row 13, second half (plan table). A bot the webhook's scope does not allow
// is one per-bot filtered row beside the bots that DO launch — never a 4xx
// that would sink the whole delivery, and never a launch of a different bot
// on its label.
//
// RED today: the GitLab half finds no per-bot filtered row for saboteur (the
// legacy election never sees its row); the GitHub half launches nestor on
// SABOTEUR's label — the legacy path has no per-bot answer at all.
func TestSpineIssueLabel_BotNotAllowedIsFilteredOthersLaunch(t *testing.T) {
	bots := []spineBotDecl{
		{bot: "nestor", mode: bundle.ExecutionDirect, labels: []string{"nestor"}},
		{bot: "saboteur", mode: bundle.ExecutionDirect, labels: []string{"saboteur"}},
	}
	t.Run("gitlab, the allowed bot still launches", func(t *testing.T) {
		f := newSpineRollGitLabDirectFixture(t, bots...)
		f.spineFlip(t, bots...)
		// saboteur is out of the webhook's scope; the event claims one label
		// of each.
		f.cfg.BotIDs = []string{"nestor"}
		w := f.deliver(f.body("nestor", "saboteur"), "d-1")
		if w.Code != http.StatusAccepted {
			t.Fatalf("a delivery one allowed bot claims: code=%d body=%s, want 202 with nestor launched and saboteur filtered", w.Code, w.Body.String())
		}
		// The filtered bot is never a target, so the answer is the
		// single-target shape: the launch reads from the tail's recording.
		if got := f.launchesByBot("nestor"); got != 1 {
			t.Fatalf("nestor's launches = %d, want 1", got)
		}
		if got := f.launchesByBot("saboteur"); got != 0 {
			t.Fatalf("saboteur's launches = %d, want 0 — it is out of the webhook's scope", got)
		}
		filtered := false
		for _, r := range spineDeliveryRowsByBot(t, f, "saboteur") {
			if r.Status == webhooks.StatusFiltered && r.Error != "" {
				filtered = true
			}
		}
		if !filtered {
			t.Fatalf("saboteur left no per-bot filtered row (rows: %+v) — the out-of-scope bot is filtered, never a 4xx for the delivery", spineDeliveryRowsByBot(t, f, "saboteur"))
		}
	})
	t.Run("github, the label starts no other bot", func(t *testing.T) {
		f := newSpineRollGitHubDirectFixture(t, bots...)
		f.spineFlip(t, bots...)
		f.cfg.BotIDs = []string{"nestor"}
		w := f.deliver(f.body("saboteur"), "d-1")
		resp := laneResp(t, w)
		if w.Code != http.StatusOK || resp["status"] != webhooks.StatusFiltered {
			t.Fatalf("the out-of-scope bot's own label: code=%d body=%s, want 200 filtered — never a 4xx, never another bot's launch", w.Code, w.Body.String())
		}
		if !strings.Contains(resp["reason"], "saboteur") {
			t.Fatalf("the reason = %q, want it to name the refused bot", resp["reason"])
		}
		if got := f.launchesByBot("nestor"); got != 0 {
			t.Fatalf("nestor's launches = %d, want 0 — a bot never launches on another bot's label", got)
		}
	})
}

// Row 14 (plan table). A flipped integration whose rows were never projected
// refuses the delivery as retryable: a launch_error audit row naming the sync
// route, an errtrack-visible warning, a 503 — and no legacy fallback that
// would launch a bot the operator moved past.
//
// RED today: the legacy path runs and answers the delivery (here the
// two-label-bot 403) instead of the 503.
func TestSpineIssueLabel_UnprojectedIntegration_Is503(t *testing.T) {
	bots := []spineBotDecl{
		{bot: "nestor", mode: bundle.ExecutionDirect, labels: []string{"nestor"}},
		{bot: "saboteur", mode: bundle.ExecutionDirect, labels: []string{"saboteur"}},
	}
	f := newSpineRollGitHubDirectFixture(t, bots...)
	// The flip WITHOUT the projection: the integration is bound (the config
	// names it) but its subscriptions were never synced.
	f.cfg = spineFlipConfig(f.cfg)
	log := spineCaptureWarnings(f.s)

	w := f.deliver(f.body("nestor"), "d-1")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("an unprojected integration: code=%d body=%s, want 503 — the flip without rows is a sync problem, not a legacy fallback", w.Code, w.Body.String())
	}
	var row *webhooks.Delivery
	for _, r := range f.spineAllDeliveryRows(t) {
		if r.Status == webhooks.StatusLaunchError && strings.Contains(r.Error, spineIntegrationID) && strings.Contains(r.Error, "subscriptions/sync") {
			row = &r
		}
	}
	if row == nil {
		t.Fatalf("no launch_error row names the integration and its sync route (rows: %+v)", f.spineAllDeliveryRows(t))
	}
	if got := f.launchesByBot("nestor") + f.launchesByBot("saboteur"); got != 0 {
		t.Fatalf("launches = %d, want 0", got)
	}
	if !strings.Contains(log.String(), "no projected subscriptions") {
		t.Fatalf("the server logged %q, want an errtrack-visible warning naming the integration's missing subscriptions", log.String())
	}
}

// Row 15 (plan table). A lane the server cannot read refuses the WHOLE
// delivery as retryable before anything is claimed: bot A is not claimed
// because bot B's lane read failed — the read phase precedes the claim phase.
//
// RED today: the legacy path answers the two-label-bot 403.
func TestSpineIssueLabel_LaneUnreadable_Is503NothingClaimed(t *testing.T) {
	bots := []spineBotDecl{
		{bot: "nestor", mode: bundle.ExecutionDirect, labels: []string{"nestor", "*-chaos"}},
		{bot: "saboteur", mode: bundle.ExecutionBoard, labels: []string{"saboteur", "*-chaos"}},
	}
	f := newSpineRollGitHubFixture(t, bots...)
	f.spineFlip(t, bots...)
	f.s.cfg.CloudBoardFor = func(string) native.BoardStore { return failingBoard{f.board} }

	w := f.deliver(f.body("run-chaos"), "d-1")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("a lane the server cannot read: code=%d body=%s, want 503 for the whole delivery", w.Code, w.Body.String())
	}
	if got := f.launchesByBot("nestor") + f.launchesByBot("saboteur"); got != 0 {
		t.Fatalf("launches = %d, want 0 — B's failed read must not leave A claimed", got)
	}
	if n := len(laneCards(t, f.board)); n != 0 {
		t.Fatalf("cards = %d, want 0", n)
	}
	claimed := 0
	var row *webhooks.Delivery
	for _, r := range f.spineAllDeliveryRows(t) {
		if r.SubscriptionID != "" {
			claimed++
		}
		if r.Status == webhooks.StatusLaunchError && strings.Contains(r.Error, "trigger lane unavailable") {
			row = &r
		}
	}
	if claimed != 0 {
		t.Fatalf("%d audit rows carry a subscription: nothing was claimed", claimed)
	}
	if row == nil {
		t.Fatalf("no launch_error row names the unreadable lane (rows: %+v)", f.spineAllDeliveryRows(t))
	}
}

// spineFailClaimInsert fails the CLAIM write of one bot's rows — a claim
// carries the subscription that made it, an audit row never does — so the
// mid-fan-out store failure row 16 is decided on leaves the audit channel
// working.
type spineFailClaimInsert struct {
	webhooks.DeliveryStore
	bot string
}

func (w spineFailClaimInsert) Insert(ctx context.Context, d webhooks.Delivery) error {
	if d.BotID == w.bot && d.SubscriptionID != "" {
		return errors.New("mongo: write conflict")
	}
	return w.DeliveryStore.Insert(ctx, d)
}

// Row 16 (plan table). A claim write that fails AFTER the first target answers
// 503 partial: the claimed target still launches, the unclaimed bot is named
// in a launch_error audit row, and the redelivery completes the fan-out
// (A duplicate, B launched).
//
// RED today: the legacy path answers the two-label-bot 403.
func TestSpineIssueLabel_ClaimWriteFailsAfterFirst_Is503AndClaimedStillLaunch(t *testing.T) {
	bots := []spineBotDecl{
		{bot: "nestor", mode: bundle.ExecutionDirect, labels: []string{"nestor", "*-chaos"}},
		{bot: "saboteur", mode: bundle.ExecutionDirect, labels: []string{"saboteur", "*-chaos"}},
	}
	f := newSpineRollGitHubDirectFixture(t, bots...)
	f.spineFlip(t, bots...)
	f.s.webhookDeliveries = spineFailClaimInsert{DeliveryStore: f.s.webhookDeliveries, bot: "saboteur"}

	w := f.deliver(f.body("run-chaos"), "d-1")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("a claim write failing after the first target: code=%d body=%s, want 503 partial", w.Code, w.Body.String())
	}
	if got := f.launchesByBot("nestor"); got != 1 {
		t.Fatalf("nestor's launches = %d, want 1 — the claimed target launches whatever the rest of the fan-out did", got)
	}
	if got := f.launchesByBot("saboteur"); got != 0 {
		t.Fatalf("saboteur's launches = %d, want 0 — its claim write failed", got)
	}
	var row *webhooks.Delivery
	for _, r := range f.spineAllDeliveryRows(t) {
		if r.Status == webhooks.StatusLaunchError && strings.Contains(r.Error, "saboteur") {
			row = &r
		}
	}
	if row == nil {
		t.Fatalf("no launch_error audit row names the unclaimed bot saboteur (rows: %+v)", f.spineAllDeliveryRows(t))
	}

	// The forge redelivers: nestor's claim replays as a duplicate, saboteur's
	// lane is free and launches.
	w = f.deliver(f.body("run-chaos"), "d-2")
	if w.Code != http.StatusAccepted {
		t.Fatalf("the redelivery: code=%d body=%s, want 202 with saboteur launched", w.Code, w.Body.String())
	}
	launches := spineLaunches(t, w)
	if len(launches) != 1 || launches[0]["bot"] != "saboteur" {
		t.Fatalf("launches = %v, want exactly saboteur", launches)
	}
	if got := f.launchesByBot("nestor"); got != 1 {
		t.Fatalf("nestor's launches = %d, want 1 — its claim serves, the redelivery is its duplicate", got)
	}
}

// Row 17 (plan table). The repository pre-gates a spine-routed config keeps:
// the label allowlist, the hold label, the project allowlist and the event
// allowlist refuse BEFORE any routing decision, for both providers. A flipped
// config changes none of their answers.
//
// NOT RED today: the pre-gates run before the (absent) spine branch, so the
// legacy path already answers each refusal with the very bytes this witness
// pins. Its mutation lives in the router lot: moving the spine branch BEFORE
// the hold gate reds the hold-label half (a held issue would route).
func TestSpineIssueLabel_RepoPreGatesUnchanged(t *testing.T) {
	bots := []spineBotDecl{
		{bot: "nestor", mode: bundle.ExecutionDirect, labels: []string{"nestor"}},
		{bot: "saboteur", mode: bundle.ExecutionDirect, labels: []string{"saboteur"}},
	}
	// The GitHub hold payload: the label event for nestor on an issue that
	// carries the hold label.
	ghHold := strings.Replace(ghIssueLabelBody("nestor"), `"state": "open",`,
		`"state": "open", "labels": [{"name": "iterion:hold"}],`, 1)
	for _, forgeName := range []string{"github", "gitlab"} {
		for _, gate := range []struct {
			name   string
			cfg    func(webhooks.Config) webhooks.Config
			added  []string // the payload's added labels, in order
			ghHold bool     // GitHub only: the issue carries the hold label
		}{
			{
				name:  "label allowlist",
				cfg:   func(c webhooks.Config) webhooks.Config { c.LabelAllowlist = []string{"implement"}; return c },
				added: []string{"nestor"},
			},
			{
				name:   "hold label",
				cfg:    func(c webhooks.Config) webhooks.Config { c.HoldLabels = []string{"iterion:hold"}; return c },
				added:  []string{"nestor", "iterion:hold"},
				ghHold: true,
			},
			{
				name:  "project allowlist",
				cfg:   func(c webhooks.Config) webhooks.Config { c.ProjectAllowlist = []string{"acme/other"}; return c },
				added: []string{"nestor"},
			},
			{
				name:  "event allowlist",
				cfg:   func(c webhooks.Config) webhooks.Config { c.EventAllowlist = []string{"pull_request"}; return c },
				added: []string{"nestor"},
			},
		} {
			t.Run(forgeName+"/"+gate.name, func(t *testing.T) {
				var f *spineRollFixture
				if forgeName == "gitlab" {
					f = newSpineRollGitLabDirectFixture(t, bots...)
				} else {
					f = newSpineRollGitHubDirectFixture(t, bots...)
				}
				f.cfg = gate.cfg(f.cfg)
				f.spineFlip(t, bots...)
				var body string
				if forgeName == "gitlab" {
					body = f.body(gate.added...)
				} else if gate.ghHold {
					body = ghHold
				} else {
					body = f.body(gate.added[0])
				}
				w := f.deliver(body, "d-1")
				resp := laneResp(t, w)
				if w.Code != http.StatusOK || resp["status"] != webhooks.StatusFiltered {
					t.Fatalf("the %s gate on a spine-routed config: code=%d body=%s, want 200 filtered before any routing", gate.name, w.Code, w.Body.String())
				}
				if got := f.launchesByBot("nestor") + f.launchesByBot("saboteur"); got != 0 {
					t.Fatalf("launches = %d, want 0 — the pre-gate refuses before any routing", got)
				}
				if n := len(f.spineAllDeliveryRows(t)); n != 1 {
					t.Fatalf("audit rows = %d, want exactly the pre-gate refusal", n)
				}
			})
		}
	}
}

// Row 37, first half (plan table). A board target the spine claimed leaves a
// `carded` audit row carrying the card and the subscription that claimed the
// event — the attribution the direct lane's rows already carry.
//
// RED today: the legacy path answers the two-label-bot 403 (and writes no
// carded audit row at all).
func TestSpineIssueLabel_CardedTargetLeavesAuditRow(t *testing.T) {
	bots := []spineBotDecl{
		{bot: "nestor", mode: bundle.ExecutionDirect, labels: []string{"nestor"}},
		{bot: "saboteur", mode: bundle.ExecutionBoard, labels: []string{"saboteur"}},
	}
	f := newSpineRollGitHubFixture(t, bots...)
	f.spineFlip(t, bots...)
	w := f.deliver(f.body("saboteur"), "d-1")
	if w.Code != http.StatusAccepted {
		t.Fatalf("the board bot's label: code=%d body=%s, want 202 accepted", w.Code, w.Body.String())
	}
	cards := laneCards(t, f.board)
	if len(cards) != 1 || cards[0].Bot != "saboteur" {
		t.Fatalf("cards = %+v, want one saboteur card", cards)
	}
	row := wantSpineClaimRow(t, f.spineAllDeliveryRows(t), "saboteur")
	if row.Status != webhooks.StatusCarded {
		t.Fatalf("saboteur's audit row reads status %q, want %q", row.Status, webhooks.StatusCarded)
	}
	if row.CardID != cards[0].ID {
		t.Fatalf("saboteur's audit row names card %q, want the created card %s", row.CardID, cards[0].ID)
	}
	if row.RoutedBy != webhooks.RoutingSpine {
		t.Fatalf("saboteur's audit row reads routed_by %q, want %q", row.RoutedBy, webhooks.RoutingSpine)
	}
}

// Row 37, second half (plan table), the server half: the lane reader's
// verdict never selects a carded row — the audit row of a board claim stands
// NEXT TO the lane (under the carded key), never on a chain a read walks, and
// a board claim's answering is the card's job. The store half (ListRetryDue,
// CountLaunched never select it) lives in pkg/webhooks'
// delivery_carded_test.go.
//
// NOT RED today: the reader is already written and correct; this witness pins
// the invariant the router lot must keep when it writes the audit row (its
// mutation — writing the row as `accepted` onto the chain — reds it).
func TestDeliveryCarded_NeverClaimedRetriedOrCounted(t *testing.T) {
	const cardID = "native:5f1d2c8e-3b4a-5e7f-9a0b-c1d2e3f4a5b6"
	base := botLaneBase(ghBase("nestor"), "nestor")
	cardedRow := func() webhooks.Delivery {
		return webhooks.Delivery{
			ID: "row-carded", IdempotencyKey: knowledge.ChecksumHex([]byte(base + "|carded|" + cardID)),
			BotID: "nestor", Status: webhooks.StatusCarded, CardID: cardID,
			SubscriptionID: "sub-nestor-1", RoutedBy: webhooks.RoutingSpine,
			PayloadHash: "hash-e", ForgeDeliveryID: "d-e", ReceivedAt: laneNow,
		}
	}

	t.Run("the direct read", func(t *testing.T) {
		s := newWebhookTestServer(t)
		laneSeedRow(t, s, cardedRow())
		v := readBotLane(t, s, laneCfg(), botLane("nestor", "nestor"),
			laneEvent{PayloadHash: "hash-f", DeliveryID: "d-f"}, laneRead{Bot: "nestor"})
		wantLaneFree(t, v, "the lane beside a carded audit row", base)
	})
	t.Run("the board read", func(t *testing.T) {
		s, board, _ := newBoardLaneServer(t)
		laneSeedRow(t, s, cardedRow())
		ln := botLane("nestor", "nestor")
		v := readBotLane(t, s, laneCfg(), ln,
			laneEvent{PayloadHash: "hash-f", DeliveryID: "d-f"}, laneRead{Bot: "nestor", Board: true})
		if v.answered != nil || v.card != nil {
			t.Fatalf("the board read answered %+v card %+v, want nothing — a carded audit row is not a claim", v.answered, v.card)
		}
		if want := laneCardID("t1", ln, nil); v.nextCardID != want {
			t.Fatalf("nextCardID = %q, want the empty-set id %q — the audit row must not sit in the held set", v.nextCardID, want)
		}
		if n := len(laneCards(t, board)); n != 0 {
			t.Fatalf("cards = %d, want 0", n)
		}
	})
}

// Row 40, server half (plan table). Two rows of ONE bot claiming the same
// label — a shape the projection refuses and only a hand edit (or an older
// planner) produces — filter THAT bot with one ambiguous reason naming both
// rows, and start nothing: never two runs, never two cards.
//
// RED today: the legacy path elects the sole bot and launches it.
func TestSpineIssueLabel_OneBotTwoRows_AmbiguousNotTwoRuns(t *testing.T) {
	bots := []spineBotDecl{
		{bot: "nestor", mode: bundle.ExecutionDirect, labels: []string{"nestor"}},
		{bot: "saboteur", mode: bundle.ExecutionBoard, labels: []string{"saboteur"}},
	}
	f := newSpineRollGitHubFixture(t, bots...)
	f.cfg = spineFlipConfig(f.cfg)
	rows := spineProjectCatalogueRows(t, f.s, f.cfg, bots...)
	var nestor trigger.Subscription
	for _, r := range rows {
		if r.BotID == "nestor" {
			nestor = r
		}
	}
	// The hand-written twin: same bot, same labels, the other mode — the
	// overlap the projector refuses.
	twin := nestor
	twin.ID = nestor.ID + "-twin"
	twin.Mode = bundle.ExecutionBoard
	if err := f.s.cfg.TriggerStore.Create(context.Background(), twin); err != nil {
		t.Fatalf("store the twin row: %v", err)
	}

	w := f.deliver(f.body("nestor"), "d-1")
	resp := laneResp(t, w)
	if w.Code != http.StatusOK || resp["status"] != webhooks.StatusFiltered {
		t.Fatalf("two rows of one bot on one label: code=%d body=%s, want 200 filtered with the ambiguous reason", w.Code, w.Body.String())
	}
	if reason := resp["reason"]; !strings.Contains(reason, "ambiguous") ||
		!strings.Contains(reason, nestor.ID) || !strings.Contains(reason, twin.ID) {
		t.Fatalf("the reason = %q, want the ambiguity named with both rows %s and %s", reason, nestor.ID, twin.ID)
	}
	if got := f.launchesByBot("nestor"); got != 0 {
		t.Fatalf("nestor's launches = %d, want 0 — an ambiguous bot starts nothing", got)
	}
	if n := len(laneCards(t, f.board)); n != 0 {
		t.Fatalf("cards = %d, want 0", n)
	}
}

// Row 41, server half (plan table). A PATCH of launch_vars on a MANAGED
// config whose routing sends ANY lane to the spine answers a typed 409
// launch_vars_on_spine naming the hand pin: on a spine-routed config the
// config's LaunchVars must stay the manifest union, or the pins would leak
// across bots (ADR-172 §6). A managed config on full legacy still accepts the
// patch.
//
// RED today: the current CRUD answer applies the patch (200).
func TestWebhookPatch_LaunchVarsRefusedOnSpineConfig(t *testing.T) {
	patch := func(t *testing.T, s *Server, id string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		s.handleUpdateWebhook(w, whReq(superAdminCtx(), "PATCH", "/api/teams/t1/webhooks/"+id,
			`{"launch_vars":{"scope":"hand-pinned"}}`, "t1", id))
		return w
	}
	for _, tc := range []struct {
		name    string
		routing webhooks.ForgeRouting
	}{
		{name: "the issues lane on the spine", routing: webhooks.ForgeRouting{Issues: webhooks.RoutingSpine}},
		{name: "only the pull_requests lane on the spine", routing: webhooks.ForgeRouting{PullRequests: webhooks.RoutingSpine}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newWebhookTestServer(t)
			since := laneNow
			cfg := webhooks.Config{
				ID: "w1", TenantID: "t1", Provider: webhooks.ProviderGitHub, Enabled: true,
				BotIDs: []string{"nestor"},
				// Managed by the integration, and routed to the spine on one lane.
				ProvisionIntegrationID: spineIntegrationID,
				ForgeRouting:           tc.routing,
				SpineSince:             &since,
			}
			if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			w := patch(t, s, "w1")
			if w.Code != http.StatusConflict {
				t.Fatalf("launch_vars on a spine-routed managed config: code=%d body=%s, want 409 launch_vars_on_spine", w.Code, w.Body.String())
			}
			body := w.Body.String()
			if !strings.Contains(body, "launch_vars_on_spine") {
				t.Fatalf("the refusal body = %s, want the typed launch_vars_on_spine reason", body)
			}
			if !strings.Contains(body, "scope") {
				t.Fatalf("the refusal body = %s, want it to name the hand pin it refuses", body)
			}
			stored, err := s.webhookConfigs.Get(context.Background(), "w1")
			if err != nil {
				t.Fatal(err)
			}
			if len(stored.LaunchVars) != 0 {
				t.Fatalf("the patch landed anyway (%v): a refused hand pin must not write", stored.LaunchVars)
			}
		})
	}
	t.Run("a managed config on full legacy still patches", func(t *testing.T) {
		s := newWebhookTestServer(t)
		cfg := webhooks.Config{
			ID: "w1", TenantID: "t1", Provider: webhooks.ProviderGitHub, Enabled: true,
			BotIDs:                 []string{"nestor"},
			ProvisionIntegrationID: spineIntegrationID,
		}
		if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		w := patch(t, s, "w1")
		if w.Code != http.StatusOK {
			t.Fatalf("launch_vars on a legacy managed config: code=%d body=%s, want 200", w.Code, w.Body.String())
		}
		stored, err := s.webhookConfigs.Get(context.Background(), "w1")
		if err != nil {
			t.Fatal(err)
		}
		if stored.LaunchVars["scope"] != "hand-pinned" {
			t.Fatalf("the legacy patch did not apply: %v", stored.LaunchVars)
		}
	})
}

// Row 42 (plan table). The staleness gate: rows projected from a catalogue
// that has since changed route NOTHING — a 503 subscriptions_stale with a
// launch_error row naming the sync route, an errtrack-visible warning, no
// launch and no legacy fallback — and a re-sync routes again.
//
// RED today: the legacy path runs (here the two-label-bot 403) instead of the
// staleness refusal.
func TestSpineIssueLabel_StaleDigest_Is503(t *testing.T) {
	nestor := spineBotDecl{bot: "nestor", mode: bundle.ExecutionDirect, labels: []string{"nestor"}}
	saboteur := spineBotDecl{bot: "saboteur", mode: bundle.ExecutionDirect, labels: []string{"saboteur"}}
	f := newSpineRollGitHubFixture(t, nestor, saboteur)
	f.spineFlip(t, nestor, saboteur)

	// While the rows match the catalogue, the label routes.
	w := f.deliver(f.body("nestor"), "d-1")
	if w.Code != http.StatusAccepted {
		t.Fatalf("a label the fresh projection claims: code=%d body=%s, want 202 accepted", w.Code, w.Body.String())
	}
	if got := f.launchesByBot("nestor"); got != 1 {
		t.Fatalf("nestor's launches = %d, want 1", got)
	}

	// The catalogue drifts: nestor's manifest claims another label now.
	drifted := nestor
	drifted.labels = []string{"renamed"}
	spineWriteCatalogueBots(t, f.s, drifted)
	log := spineCaptureWarnings(f.s)

	w = f.deliver(f.body("nestor"), "d-2")
	resp := laneResp(t, w)
	if w.Code != http.StatusServiceUnavailable || resp["status"] != "subscriptions_stale" {
		t.Fatalf("a delivery on stale rows: code=%d body=%s, want 503 subscriptions_stale", w.Code, w.Body.String())
	}
	var row *webhooks.Delivery
	for _, r := range f.spineAllDeliveryRows(t) {
		if r.Status == webhooks.StatusLaunchError && strings.Contains(r.Error, "subscriptions/sync") {
			row = &r
		}
	}
	if row == nil {
		t.Fatalf("no launch_error row names the sync route (rows: %+v)", f.spineAllDeliveryRows(t))
	}
	if !strings.Contains(log.String(), "subscriptions") {
		t.Fatalf("the server logged %q, want an errtrack-visible warning naming the stale subscriptions", log.String())
	}
	if got := f.launchesByBot("nestor"); got != 1 {
		t.Fatalf("nestor's launches = %d, want 1 — stale rows launch nothing", got)
	}
	if n := len(laneCards(t, f.board)); n != 0 {
		t.Fatalf("cards = %d, want 0 — stale rows never fall back to legacy", n)
	}

	// The re-sync re-projects from the drifted catalogue; the new label routes.
	spineProjectCatalogueRows(t, f.s, f.cfg, drifted, saboteur)
	w = f.deliver(f.body("renamed"), "d-3")
	if w.Code != http.StatusAccepted {
		t.Fatalf("after the re-sync: code=%d body=%s, want 202 accepted", w.Code, w.Body.String())
	}
	launches := spineLaunches(t, w)
	if len(launches) != 1 || launches[0]["bot"] != "nestor" {
		t.Fatalf("launches = %v, want exactly nestor on its re-projected label", launches)
	}
	if got := f.launchesByBot("nestor"); got != 2 {
		t.Fatalf("nestor's launches = %d, want 2 — the re-synced projection routes", got)
	}
}

// The digest a delivery recomputes covers the SAME bot set the projector
// planned over: the integration's whole bot scope, a bot that claims no issue
// event included (its manifest is hashed as a bot with no route). A router
// that hashed only the bots of its bound rows would hold every row of such an
// integration stale — a repo carrying a reviewer beside its label bots would
// answer 503 subscriptions_stale on every label, forever.
func TestSpineIssueLabel_DigestCoversTheBotsThatClaimNoIssueEvent(t *testing.T) {
	nestor := spineBotDecl{bot: "nestor", mode: bundle.ExecutionDirect, labels: []string{"nestor"}}
	saboteur := spineBotDecl{bot: "saboteur", mode: bundle.ExecutionDirect, labels: []string{"saboteur"}}
	f := newSpineRollGitHubFixture(t, nestor, saboteur)

	// A pull-request bot on the same integration: it derives no issue row.
	dir := filepath.Join(f.s.cfg.WorkDir, "bots", "pr-reviewer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "name: pr-reviewer\nschema_version: 1\ninvocations:\n  - kind: forge\n    forge:\n      event: pull_request\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte("agent x:\n  model: \"test\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.cfg.BotIDs = append(f.cfg.BotIDs, "pr-reviewer")
	f.spineFlip(t, nestor, saboteur)

	w := f.deliver(f.body("nestor"), "d-1")
	if w.Code != http.StatusAccepted {
		t.Fatalf("a label on an integration carrying a pull-request bot: code=%d body=%s, want 202 — the digest must cover the integration's whole bot set", w.Code, w.Body.String())
	}
	if got := f.launchesByBot("nestor"); got != 1 {
		t.Fatalf("nestor's launches = %d, want 1", got)
	}
}
