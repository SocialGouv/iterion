package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// failBotDeliveries fails every Delivery write of one bot while armed — the
// claim AND the parked retry row: a store outage, not a single write.
type failBotDeliveries struct {
	webhooks.DeliveryStore
	bot   string
	armed *atomic.Bool
}

func (s failBotDeliveries) Insert(ctx context.Context, d webhooks.Delivery) error {
	if s.armed.Load() && d.BotID == s.bot {
		return errors.New("mongo: not primary")
	}
	return s.DeliveryStore.Insert(ctx, d)
}

// failBotCards fails every card create of one bot while armed.
type failBotCards struct {
	native.BoardStore
	bot   string
	armed *atomic.Bool
}

func (b failBotCards) Create(in native.Issue) (*native.Issue, error) {
	if b.armed.Load() && in.Bot == b.bot {
		return nil, errors.New("mongo: socket closed")
	}
	return b.BoardStore.Create(in)
}

// RT-F1, carded sibling: a fan-out whose board bot carded and whose direct
// bot's claim (and its parked retry row) failed answers 503 partial — and the
// redelivery that answer asks for starts the direct bot. The card carded for
// the event answers its own bot only, never the whole delivery.
//
// Mutation: in routeIssueThroughSpine's read phase, a spine-era record
// answers the whole delivery (eraAnswer set whatever verdict.legacyEra says)
// → red on "the redelivery never started saboteur".
func TestSpineIssueLabel_PartialRedeliveryStartsTheBotACardedSiblingLeft(t *testing.T) {
	bots := []spineBotDecl{
		{bot: "nestor", mode: bundle.ExecutionBoard, labels: []string{"*-chaos"}},
		{bot: "saboteur", mode: bundle.ExecutionDirect, labels: []string{"*-chaos"}},
	}
	f := newSpineRollGitHubFixture(t, bots...)
	f.spineFlip(t, bots...)
	armed := &atomic.Bool{}
	armed.Store(true)
	f.s.webhookDeliveries = failBotDeliveries{DeliveryStore: f.s.webhookDeliveries, bot: "saboteur", armed: armed}

	w := f.deliver(f.body("run-chaos"), "d-1")
	if w.Code != http.StatusServiceUnavailable || laneRespAny(t, w)["status"] != "partial" {
		t.Fatalf("first delivery: code=%d body=%s, want 503 partial (nestor carded, saboteur unclaimed)", w.Code, w.Body.String())
	}
	armed.Store(false)
	w = f.deliver(f.body("run-chaos"), "d-1")
	if w.Code != http.StatusAccepted {
		t.Fatalf("the redelivery: code=%d body=%s, want 202 with saboteur launched", w.Code, w.Body.String())
	}
	if got := f.launchesByBot("saboteur"); got != 1 {
		t.Fatalf("saboteur launches = %d: the redelivery never started saboteur, want 1", got)
	}
	if n := len(laneCards(t, f.board)); n != 1 {
		t.Fatalf("cards = %d, want nestor's one card — the redelivery cards nothing twice", n)
	}
}

// RT-F1, claimed and absorbed sibling: a fresh application whose first bot
// is absorbed by its serving claim and whose second bot's claim failed is
// completed by its redelivery; the absorption record answers its own bot.
//
// Mutation: as above → red on "the redelivery never started saboteur".
func TestSpineIssueLabel_PartialRedeliveryStartsTheBotAnAbsorbedSiblingLeft(t *testing.T) {
	bots := []spineBotDecl{
		{bot: "nestor", mode: bundle.ExecutionDirect, labels: []string{"*-chaos"}},
		{bot: "saboteur", mode: bundle.ExecutionDirect, labels: []string{"*-chaos"}},
	}
	f := newSpineRollGitHubDirectFixture(t, bots...)
	f.spineFlip(t, bots...)
	wantLaunched(t, f.deliver(f.body("run-chaos"), "d-0"), "the first application")
	// saboteur's run ends, nestor's still serves.
	rows, err := f.s.webhookDeliveries.ListByWebhook(context.Background(), f.cfg.TenantID, f.cfg.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.BotID == "saboteur" && r.RunID != "" {
			f.runs.end(r.RunID)
		}
	}
	armed := &atomic.Bool{}
	armed.Store(true)
	f.s.webhookDeliveries = failBotDeliveries{DeliveryStore: f.s.webhookDeliveries, bot: "saboteur", armed: armed}
	before := f.launchesByBot("saboteur")
	if w := f.deliver(f.body("run-chaos"), "d-1"); w.Code < http.StatusInternalServerError {
		t.Fatalf("the second application with saboteur's store down: code=%d body=%s, want a retryable 5xx", w.Code, w.Body.String())
	}
	armed.Store(false)
	w := f.deliver(f.body("run-chaos"), "d-1")
	if w.Code != http.StatusAccepted {
		t.Fatalf("the redelivery: code=%d body=%s, want 202 with saboteur launched", w.Code, w.Body.String())
	}
	if got := f.launchesByBot("saboteur"); got != before+1 {
		t.Fatalf("saboteur launches = %d after the redelivery, want %d: the redelivery never started saboteur", got, before+1)
	}
	if got := f.launchesByBot("nestor"); got != 1 {
		t.Fatalf("nestor launches = %d, want 1 — absorbed by its serving claim, twice", got)
	}
}

// RT-F2: a board bot whose card write fails mid claim phase is a per-bot
// claim failure — the board bot before it stays carded, the direct bot queued
// after it still claims, the answer is 503 partial naming what claimed, and
// the redelivery cards the failed bot without starting the others twice.
//
// Mutation: a cardTriggerLaneDecide error answers laneUnavailable and
// returns (the pre-fix shape) → red on "gamma launches = 0".
func TestSpineIssueLabel_BoardClaimFailureIsOneBotsFailure(t *testing.T) {
	bots := []spineBotDecl{
		{bot: "alpha", mode: bundle.ExecutionBoard, labels: []string{"go"}},
		{bot: "beta", mode: bundle.ExecutionBoard, labels: []string{"go"}},
		{bot: "gamma", mode: bundle.ExecutionDirect, labels: []string{"go"}},
	}
	f := newSpineRollGitHubFixture(t, bots...)
	f.spineFlip(t, bots...)
	armed := &atomic.Bool{}
	armed.Store(true)
	fb := failBotCards{BoardStore: f.board, bot: "beta", armed: armed}
	f.s.cfg.CloudBoardFor = func(string) native.BoardStore { return fb }

	w := f.deliver(f.body("go"), "d-1")
	if w.Code != http.StatusServiceUnavailable || laneRespAny(t, w)["status"] != "partial" {
		t.Fatalf("first delivery: code=%d body=%s, want 503 partial (alpha carded, gamma launched, beta unclaimed)", w.Code, w.Body.String())
	}
	if got := f.launchesByBot("gamma"); got != 1 {
		t.Fatalf("gamma launches = %d after the first delivery, want 1 — a sibling's card failure never strands it", got)
	}
	armed.Store(false)
	w = f.deliver(f.body("go"), "d-1")
	if w.Code != http.StatusAccepted {
		t.Fatalf("the redelivery: code=%d body=%s, want 202 with beta carded", w.Code, w.Body.String())
	}
	byBot := map[string]int{}
	for _, c := range laneCards(t, f.board) {
		byBot[c.Bot]++
	}
	if byBot["alpha"] != 1 || byBot["beta"] != 1 {
		t.Fatalf("cards by bot = %v, want one alpha card and one beta card", byBot)
	}
	if got := f.launchesByBot("gamma"); got != 1 {
		t.Fatalf("gamma launches = %d after the redelivery, want 1", got)
	}
}

// laneRespAny decodes a JSON answer whose values are not all strings (a
// launches array beside the status).
func laneRespAny(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response %d is not JSON: %s", w.Code, w.Body.String())
	}
	return resp
}

// R2-F2: a claim that failed while nothing claimed answers the retryable 503
// partial, with the launch_error audit row naming the unclaimed bot — never
// the first denial (a 4xx the forge does not redeliver, beta unnamed), and
// never a 502 that names nobody in the audit. Two producers feed the tail's
// claim failures: a board claim the router could not decide (it rides pre),
// and a claim row the tail itself could not write.
//
// Mutation: acceptWebhookTargetsCore answers the partial only when another
// target claimed (the `len(claimed) > 0 || len(launches) > 0` guard) → red
// on "want 503 partial" in every case.
func TestSpineTail_ClaimFailureWithNothingClaimedIs503Partial(t *testing.T) {
	bots := []spineBotDecl{
		{bot: "alpha", mode: bundle.ExecutionBoard, labels: []string{"go"}},
		{bot: "beta", mode: bundle.ExecutionDirect, labels: []string{"go"}},
	}
	denied := webhookLaunchResult{BotID: "alpha", Status: webhooks.StatusLaunchError,
		denial: &launchDenial{status: http.StatusTooManyRequests, reason: "concurrency_floor", detail: "alpha: reserved slots held by other bots"}}
	duplicate := webhookLaunchResult{BotID: "alpha", Status: webhooks.StatusDuplicate, CardID: "card-alpha", SubscriptionID: "sub-alpha"}
	undecided := webhookLaunchResult{BotID: "beta", Status: webhooks.StatusLaunchError, Error: "card the trigger: mongo: socket closed",
		SubscriptionID: "sub-beta", httpStatus: http.StatusServiceUnavailable}
	betaTarget := forgeLaunchTarget{BotID: "beta", IdemKey: botLaneBase(ghBase("go"), "beta"),
		Vars: map[string]string{"feature_prompt": "go"}, subscriptionID: "sub-beta", routedBy: webhooks.RoutingSpine}
	for _, tc := range []struct {
		name    string
		pre     []webhookLaunchResult
		targets []forgeLaunchTarget
	}{
		{"an undecided board claim beside a denial", []webhookLaunchResult{denied, undecided}, nil},
		{"a claim write the tail failed beside a denial", []webhookLaunchResult{denied}, []forgeLaunchTarget{betaTarget}},
		{"an undecided board claim beside a duplicate", []webhookLaunchResult{duplicate, undecided}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSpineRollGitHubFixture(t, bots...)
			f.spineFlip(t, bots...)
			f.s.webhookDeliveries = spineFailClaimInsert{DeliveryStore: f.s.webhookDeliveries, bot: "beta"}
			w := httptest.NewRecorder()
			ctx := ghCtx(f.cfg)
			r := httptest.NewRequest(http.MethodPost, "/api/webhooks/github", nil).WithContext(ctx)
			f.s.acceptWebhookTargetsWith(ctx, w, r, f.cfg,
				webhookEventMeta{ProjectPath: spineRepo, SubjectID: "issue:42", DeliveryID: "d-1"}, tc.targets, tc.pre, "hash", "203.0.113.7")
			if w.Code != http.StatusServiceUnavailable || laneRespAny(t, w)["status"] != "partial" {
				t.Fatalf("code=%d body=%s, want 503 partial: beta's claim is undecided, so the delivery stays retryable", w.Code, w.Body.String())
			}
			var named bool
			for _, row := range f.spineAllDeliveryRows(t) {
				if row.Status == webhooks.StatusLaunchError && strings.Contains(row.Error, "not claimed: beta") {
					named = true
				}
			}
			if !named {
				t.Fatalf("no launch_error audit row names the unclaimed bot beta (rows: %+v)", f.spineAllDeliveryRows(t))
			}
			if got := f.launchesByBot("beta"); got != 0 {
				t.Fatalf("beta launches = %d, want 0 — its claim failed", got)
			}
		})
	}
}

// R2-F5: the 202's launches array names each bot's outcome, the bots this
// delivery answered as duplicates included — a redelivery that completes a
// partial fan-out reads the bot that already serves beside the one it
// starts (a launch reads "accepted": the array is written before the
// launches run).
//
// Mutation: routeIssueThroughSpine hands the tail pre and the unclaimed
// bots without the duplicates → red on "want nestor's duplicate".
func TestSpineIssueLabel_LaunchesNameTheDuplicates(t *testing.T) {
	t.Run("a direct sibling's serving claim", func(t *testing.T) {
		// The forge redelivers under a new delivery id: nestor's claim of
		// the first still serves and absorbs it, saboteur's lane is free.
		bots := []spineBotDecl{
			{bot: "nestor", mode: bundle.ExecutionDirect, labels: []string{"nestor", "*-chaos"}},
			{bot: "saboteur", mode: bundle.ExecutionDirect, labels: []string{"saboteur", "*-chaos"}},
		}
		f := newSpineRollGitHubDirectFixture(t, bots...)
		f.spineFlip(t, bots...)
		healthy := f.s.webhookDeliveries
		f.s.webhookDeliveries = spineFailClaimInsert{DeliveryStore: healthy, bot: "saboteur"}
		if w := f.deliver(f.body("run-chaos"), "d-1"); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("first delivery: code=%d body=%s, want 503 partial", w.Code, w.Body.String())
		}
		f.s.webhookDeliveries = healthy
		w := f.deliver(f.body("run-chaos"), "d-2")
		if w.Code != http.StatusAccepted {
			t.Fatalf("the redelivery: code=%d body=%s, want 202", w.Code, w.Body.String())
		}
		wantOutcomes(t, w, map[string]string{"nestor": webhooks.StatusDuplicate, "saboteur": webhooks.StatusAccepted})
	})
	t.Run("a board sibling's card", func(t *testing.T) {
		bots := []spineBotDecl{
			{bot: "nestor", mode: bundle.ExecutionBoard, labels: []string{"*-chaos"}},
			{bot: "saboteur", mode: bundle.ExecutionDirect, labels: []string{"*-chaos"}},
		}
		f := newSpineRollGitHubFixture(t, bots...)
		f.spineFlip(t, bots...)
		armed := &atomic.Bool{}
		armed.Store(true)
		f.s.webhookDeliveries = failBotDeliveries{DeliveryStore: f.s.webhookDeliveries, bot: "saboteur", armed: armed}
		if w := f.deliver(f.body("run-chaos"), "d-1"); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("first delivery: code=%d body=%s, want 503 partial", w.Code, w.Body.String())
		}
		armed.Store(false)
		w := f.deliver(f.body("run-chaos"), "d-1")
		if w.Code != http.StatusAccepted {
			t.Fatalf("the redelivery: code=%d body=%s, want 202", w.Code, w.Body.String())
		}
		launches := wantOutcomes(t, w, map[string]string{"nestor": webhooks.StatusDuplicate, "saboteur": webhooks.StatusAccepted})
		if launches["nestor"]["card_id"] == "" {
			t.Fatalf("nestor's duplicate = %v, want the card that answers it", launches["nestor"])
		}
	})
}

// wantOutcomes reads a launches array and requires exactly these bots, each
// with its status; it returns the entries by bot.
func wantOutcomes(t *testing.T, w *httptest.ResponseRecorder, want map[string]string) map[string]map[string]string {
	t.Helper()
	byBot := map[string]map[string]string{}
	for _, l := range spineLaunches(t, w) {
		byBot[l["bot"]] = l
	}
	if len(byBot) != len(want) {
		t.Fatalf("launches = %v, want one entry per bot of %v (want nestor's duplicate beside saboteur's claim)", byBot, want)
	}
	for bot, status := range want {
		if byBot[bot]["status"] != status {
			t.Fatalf("launches = %v, want %s %s", byBot, bot, status)
		}
	}
	return byBot
}

// R2-F5: a redelivery every bot of which a card answers names the bot in its
// carded body, as the legacy carded writer always did.
//
// Mutation: writeSpineAnswered drops "bot" from the carded body → red on
// "want the bot named".
func TestSpineIssueLabel_CardedDuplicateNamesItsBot(t *testing.T) {
	bots := []spineBotDecl{{bot: "nestor", mode: bundle.ExecutionBoard, labels: []string{"go"}}}
	f := newSpineRollGitHubFixture(t, bots...)
	f.spineFlip(t, bots...)
	if w := f.deliver(f.body("go"), "d-1"); w.Code != http.StatusAccepted {
		t.Fatalf("first delivery: code=%d body=%s, want 202 carded", w.Code, w.Body.String())
	}
	w := f.deliver(f.body("go"), "d-1")
	resp := laneRespAny(t, w)
	if w.Code != http.StatusOK || resp["status"] != "carded" || resp["card_id"] == "" || resp["bot"] != "nestor" {
		t.Fatalf("the redelivery: code=%d body=%s, want 200 carded with the card and the bot named", w.Code, w.Body.String())
	}
}
