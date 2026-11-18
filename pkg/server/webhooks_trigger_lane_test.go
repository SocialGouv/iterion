package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/boardmongo"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/knowledge"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/gitlab"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

// The trigger lane (#3373): an issue label is a request a person can make
// again. These tests drive the real provider handlers through the four
// promises of webhooks_trigger_lane.go, on the direct lane (no tenant board:
// the launch tail claims a delivery row) and on the board lane (a board
// dispatcher: the card is the claim), for both forges that carry an
// issue-label trigger.

// laneRuns is the run side of the direct lane: every launch mints a live
// run; a test ends it explicitly.
type laneRuns struct {
	mu       sync.Mutex
	launched []string
	live     map[string]bool
	canceled []string
}

func (lr *laneRuns) launch() string {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	id := fmt.Sprintf("run-%d", len(lr.launched)+1)
	lr.launched = append(lr.launched, id)
	lr.live[id] = true
	return id
}

func (lr *laneRuns) end(id string) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	lr.live[id] = false
}

func (lr *laneRuns) count() int {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return len(lr.launched)
}

// laneForge drives one forge's issue-label handler.
type laneForge struct {
	name string
	// labeled is the forge's label event for issue 42 of acme/widgets at the
	// given issue update instant: a fresh application of the label moves it,
	// a redelivery repeats the bytes.
	labeled func(updatedAt, label string) string
	// bind mints the forge's issue-label webhook on s (overlap is its
	// overlap policy) and returns the delivery function every post of a test
	// shares, so each one is signed by the same secret. A delivery id, when
	// given, is sent in the forge's own delivery header.
	bind func(t *testing.T, s *Server, overlap string) func(body string, deliveryID ...string) *httptest.ResponseRecorder
}

func laneForges() []laneForge {
	return []laneForge{
		{
			name: "github",
			labeled: func(updatedAt, label string) string {
				body := strings.Replace(ghLabeledIssue, `"state": "open",`, `"state": "open", "updated_at": "`+updatedAt+`",`, 1)
				return strings.Replace(body, `"label": {"name": "implement"}`, `"label": {"name": "`+label+`"}`, 1)
			},
			bind: func(t *testing.T, s *Server, overlap string) func(string, ...string) *httptest.ResponseRecorder {
				t.Helper()
				cfg, pt := issueConfig(t, s)
				cfg.LabelAllowlist = []string{"implement", "bug"}
				cfg.Overlap = overlap
				return func(body string, deliveryID ...string) *httptest.ResponseRecorder {
					r := ghReq(ghCtx(cfg), body, prforge.EventHeaderIssues, pt)
					for _, id := range deliveryID {
						r.Header.Set("X-GitHub-Delivery", id)
					}
					w := httptest.NewRecorder()
					s.handleGitHubWebhook(w, r)
					return w
				}
			},
		},
		{
			name: "gitlab",
			labeled: func(updatedAt, label string) string {
				body := strings.Replace(glLabeledIssue, `"action": "update",`, `"action": "update", "updated_at": "`+updatedAt+`",`, 1)
				return strings.ReplaceAll(body, `{"title": "implement"}`, `{"title": "`+label+`"}`)
			},
			bind: func(t *testing.T, s *Server, overlap string) func(string, ...string) *httptest.ResponseRecorder {
				t.Helper()
				cfg := glIssueConfig()
				cfg.LabelAllowlist = []string{"implement", "bug"}
				cfg.Overlap = overlap
				return func(body string, deliveryID ...string) *httptest.ResponseRecorder {
					r := glReq(gitlabCtx(cfg), body, gitlab.EventHeaderIssue)
					for _, id := range deliveryID {
						r.Header.Set("Idempotency-Key", id)
					}
					w := httptest.NewRecorder()
					s.handleGitLabWebhook(w, r)
					return w
				}
			},
		},
	}
}

func newDirectLaneServer(t *testing.T) (*Server, *laneRuns, *orgusage.MemoryCounter) {
	t.Helper()
	s := newWebhookTestServer(t)
	counter := orgusage.NewMemoryCounter()
	s.orgUsage = counter
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
	return s, runs, counter
}

func monthlyRuns(t *testing.T, counter *orgusage.MemoryCounter) int64 {
	t.Helper()
	u, err := counter.Usage(context.Background(), orgusage.OrgSubject("t1"), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return int64(u.Runs)
}

func laneResp(t *testing.T, w *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response %d is not JSON: %s", w.Code, w.Body.String())
	}
	return resp
}

func wantLaunched(t *testing.T, w *httptest.ResponseRecorder, what string) {
	t.Helper()
	if w.Code != http.StatusAccepted {
		t.Fatalf("%s: code=%d body=%s, want 202 accepted", what, w.Code, w.Body.String())
	}
}

func wantDuplicate(t *testing.T, w *httptest.ResponseRecorder, what, runID string) {
	t.Helper()
	resp := laneResp(t, w)
	if w.Code != http.StatusOK || resp["status"] != webhooks.StatusDuplicate {
		t.Fatalf("%s: code=%d body=%s, want 200 duplicate", what, w.Code, w.Body.String())
	}
	if resp["run_id"] != runID {
		t.Fatalf("%s: run_id=%q, want %q", what, resp["run_id"], runID)
	}
}

// Promise 1, direct lane: a redelivery of the launched event, after its run
// has ended, stays a duplicate and charges nothing.
func TestTriggerLane_Direct_ARedeliveryAfterTheRunEndedStaysADuplicate(t *testing.T) {
	for _, f := range laneForges() {
		t.Run(f.name, func(t *testing.T) {
			s, runs, counter := newDirectLaneServer(t)
			deliver := f.bind(t, s, "")
			first := f.labeled("2026-11-18T10:00:00Z", "implement")
			wantLaunched(t, deliver(first), "first label")
			runs.end("run-1")
			wantDuplicate(t, deliver(first), "redelivery after the run ended", "run-1")
			if n := runs.count(); n != 1 {
				t.Fatalf("launches = %d, want 1 — a redelivery is the same request, not a new one", n)
			}
			if got := monthlyRuns(t, counter); got != 1 {
				t.Fatalf("monthly runs = %d, want 1 — a redelivery must never charge the quota", got)
			}
		})
	}
}

// Promise 2, direct lane: re-applying the label once the previous run has
// ended launches again — and again after that one, generation after
// generation.
func TestTriggerLane_Direct_AFreshLabelAfterTheRunEndedLaunchesAgain(t *testing.T) {
	for _, f := range laneForges() {
		t.Run(f.name, func(t *testing.T) {
			s, runs, counter := newDirectLaneServer(t)
			deliver := f.bind(t, s, "")
			wantLaunched(t, deliver(f.labeled("2026-11-18T10:00:00Z", "implement")), "first label")
			runs.end("run-1")
			wantLaunched(t, deliver(f.labeled("2026-11-18T11:00:00Z", "implement")), "the label re-applied")
			runs.end("run-2")
			wantLaunched(t, deliver(f.labeled("2026-11-18T12:00:00Z", "implement")), "the label re-applied a third time")
			if n := runs.count(); n != 3 {
				t.Fatalf("launches = %d, want 3", n)
			}
			if got := monthlyRuns(t, counter); got != 3 {
				t.Fatalf("monthly runs = %d, want 3 — one per launch", got)
			}
			// Each launch's own redelivery still replays.
			wantDuplicate(t, deliver(f.labeled("2026-11-18T11:00:00Z", "implement")), "redelivery of the second label", "run-2")
			if n := runs.count(); n != 3 {
				t.Fatalf("launches = %d after a redelivery, want 3", n)
			}
		})
	}
}

// reserialised is the same event's body as a resend may carry it: GitLab's
// "Resend request" re-serialises the stored payload, so the bytes — and the
// payload hash — move while the delivery id stays.
func reserialised(body string) string { return strings.ReplaceAll(body, ": ", ":") }

// Promise 1 by delivery id: a resend that names the same delivery is the same
// event whatever bytes it carries — on both lanes, and for an absorbed event
// too — while a new delivery id is a fresh event.
func TestTriggerLane_ARedeliveryIsKnownByItsDeliveryID(t *testing.T) {
	for _, f := range laneForges() {
		t.Run(f.name+"/direct", func(t *testing.T) {
			s, runs, counter := newDirectLaneServer(t)
			deliver := f.bind(t, s, "")
			first := f.labeled("2026-11-18T10:00:00Z", "implement")
			wantLaunched(t, deliver(first, "d-1"), "first label")
			absorbed := f.labeled("2026-11-18T10:05:00Z", "implement")
			wantDuplicate(t, deliver(absorbed, "d-2"), "a fresh label while run-1 serves", "run-1")
			runs.end("run-1")
			wantDuplicate(t, deliver(reserialised(first), "d-1"), "the first delivery resent with other bytes", "run-1")
			if w := deliver(reserialised(absorbed), "d-2"); w.Code != http.StatusOK || laneResp(t, w)["status"] != webhooks.StatusDuplicate {
				t.Fatalf("the absorbed delivery resent with other bytes: code=%d body=%s, want 200 duplicate", w.Code, w.Body.String())
			}
			if n, got := runs.count(), monthlyRuns(t, counter); n != 1 || got != 1 {
				t.Fatalf("launches = %d, monthly runs = %d, want 1 and 1", n, got)
			}
			wantLaunched(t, deliver(f.labeled("2026-11-18T11:00:00Z", "implement"), "d-3"), "a fresh label")
			rows, err := s.webhookDeliveries.ListByWebhook(context.Background(), "t1", map[string]string{"github": "ghw", "gitlab": "w1"}[f.name], 20)
			if err != nil {
				t.Fatal(err)
			}
			ids := map[string]bool{}
			for _, r := range rows {
				ids[r.ForgeDeliveryID] = true
			}
			for _, want := range []string{"d-1", "d-2", "d-3"} {
				if !ids[want] {
					t.Fatalf("no delivery row records forge delivery %s (rows: %+v)", want, rows)
				}
			}
		})
		t.Run(f.name+"/board", func(t *testing.T) {
			s, board, _ := newBoardLaneServer(t)
			deliver := f.bind(t, s, "")
			first := f.labeled("2026-11-18T10:00:00Z", "implement")
			c1 := wantCarded(t, deliver(first, "d-1"), "first label", http.StatusAccepted)
			absorbed := f.labeled("2026-11-18T10:05:00Z", "implement")
			wantCarded(t, deliver(absorbed, "d-2"), "a fresh label while the card serves", http.StatusOK)
			finishCard(t, board, c1)
			wantCarded(t, deliver(reserialised(first), "d-1"), "the first delivery resent with other bytes", http.StatusOK)
			wantCarded(t, deliver(reserialised(absorbed), "d-2"), "the absorbed delivery resent with other bytes", http.StatusOK)
			if n := len(laneCards(t, board)); n != 1 {
				t.Fatalf("cards = %d, want 1", n)
			}
			wantCarded(t, deliver(f.labeled("2026-11-18T11:00:00Z", "implement"), "d-3"), "a fresh label", http.StatusAccepted)
		})
	}
}

// The event identity survives the durable retry: a failed launch the sweeper
// relaunches keeps its delivery id on the row the retry rewrites, so the
// event's resend after that run ended is still its own duplicate.
func TestTriggerLane_Direct_ADurableRetryKeepsTheEventIdentity(t *testing.T) {
	f := laneForges()[1] // GitLab: the resend whose bytes move
	s, runs, counter := newDirectLaneServer(t)
	deliver := f.bind(t, s, "")
	cfg := glIssueConfig()
	cfg.LabelAllowlist = []string{"implement", "bug"}
	seedIntegrationConfig(t, s, cfg)
	fail := true
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		if fail {
			return "", errors.New("provider 503")
		}
		return runs.launch(), nil
	}
	first := f.labeled("2026-11-18T10:00:00Z", "implement")
	if w := deliver(first, "d-1"); w.Code != http.StatusAccepted {
		t.Fatalf("first label: code=%d body=%s, want the claim acknowledged before its launch fails", w.Code, w.Body.String())
	}
	fail = false
	s.sweepWebhookRetries(context.Background(), time.Now().Add(48*time.Hour))
	if n := runs.count(); n != 1 {
		t.Fatalf("launches = %d after the retry sweep, want 1", n)
	}
	runs.end("run-1")
	wantDuplicate(t, deliver(reserialised(first), "d-1"), "the retried delivery resent with other bytes", "run-1")
	if n, got := runs.count(), monthlyRuns(t, counter); n != 1 || got != 1 {
		t.Fatalf("launches = %d, monthly runs = %d, want 1 and 1", n, got)
	}
}

// A fresh label that retries another event's failed claim takes the row
// over; the event it displaces stays known, so that event's own resend once
// the run ended is a duplicate, not a second launch.
func TestTriggerLane_Direct_TheEventAFreshLabelDisplacesStaysADuplicate(t *testing.T) {
	f := laneForges()[1]
	s, runs, counter := newDirectLaneServer(t)
	deliver := f.bind(t, s, "")
	fail := true
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		if fail {
			return "", errors.New("provider 503")
		}
		return runs.launch(), nil
	}
	first := f.labeled("2026-11-18T10:00:00Z", "implement")
	deliver(first, "d-1")
	fail = false
	wantLaunched(t, deliver(f.labeled("2026-11-18T10:05:00Z", "implement"), "d-2"), "a fresh label retrying the failed claim")
	runs.end("run-1")
	if w := deliver(reserialised(first), "d-1"); w.Code != http.StatusOK || laneResp(t, w)["status"] != webhooks.StatusDuplicate {
		t.Fatalf("the displaced delivery resent: code=%d body=%s, want 200 duplicate", w.Code, w.Body.String())
	}
	if n, got := runs.count(), monthlyRuns(t, counter); n != 1 || got != 1 {
		t.Fatalf("launches = %d, monthly runs = %d, want 1 and 1", n, got)
	}
}

// flakyRetryClaims fails the next failed-claim takeover with a store error.
type flakyRetryClaims struct {
	webhooks.DeliveryStore
	failNext bool
}

func (f *flakyRetryClaims) ClaimFailedRetry(ctx context.Context, d webhooks.Delivery, expect int) (bool, error) {
	if f.failNext {
		f.failNext = false
		return false, errors.New("mongo: write conflict")
	}
	return f.DeliveryStore.ClaimFailedRetry(ctx, d, expect)
}

// A takeover that never happened displaces nothing: when the fresh label
// cannot take the failed claim over, the failed event's own resend still
// retries it.
func TestTriggerLane_Direct_ATakeoverThatFailsDisplacesNothing(t *testing.T) {
	f := laneForges()[1]
	s, runs, _ := newDirectLaneServer(t)
	deliver := f.bind(t, s, "")
	fail := true
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		if fail {
			return "", errors.New("provider 503")
		}
		return runs.launch(), nil
	}
	first := f.labeled("2026-11-18T10:00:00Z", "implement")
	deliver(first, "d-1")
	fail = false
	flaky := &flakyRetryClaims{DeliveryStore: s.webhookDeliveries, failNext: true}
	s.webhookDeliveries = flaky
	if w := deliver(f.labeled("2026-11-18T10:05:00Z", "implement"), "d-2"); w.Code != http.StatusInternalServerError {
		t.Fatalf("the takeover against a failing store: code=%d body=%s, want 500", w.Code, w.Body.String())
	}
	wantLaunched(t, deliver(reserialised(first), "d-1"), "the failed delivery resent after the takeover failed")
	if n := runs.count(); n != 1 {
		t.Fatalf("launches = %d, want 1", n)
	}
}

// Two delivery ids decide alone: a fresh label whose bytes happen to equal
// the previous event's is still a fresh event.
func TestTriggerLane_Direct_TwoDeliveryIDsAreTwoEventsWhateverTheBytes(t *testing.T) {
	for _, f := range laneForges() {
		t.Run(f.name, func(t *testing.T) {
			s, runs, _ := newDirectLaneServer(t)
			deliver := f.bind(t, s, "")
			body := f.labeled("2026-11-18T10:00:00Z", "implement")
			wantLaunched(t, deliver(body, "d-1"), "first label")
			runs.end("run-1")
			wantLaunched(t, deliver(body, "d-2"), "a fresh label with the same bytes and its own delivery id")
			if n := runs.count(); n != 2 {
				t.Fatalf("launches = %d, want 2", n)
			}
		})
	}
}

// A record written for an event the forge sent no id with is still found
// when that event comes back carrying one: the payload decides when an id is
// missing on either side — for the absorbed row and for the card marker.
func TestTriggerLane_ARecordWithoutADeliveryIDMatchesByItsPayload(t *testing.T) {
	f := laneForges()[0]
	t.Run("direct", func(t *testing.T) {
		s, runs, _ := newDirectLaneServer(t)
		deliver := f.bind(t, s, "")
		wantLaunched(t, deliver(f.labeled("2026-11-18T10:00:00Z", "implement")), "first label")
		absorbed := f.labeled("2026-11-18T10:05:00Z", "implement")
		wantDuplicate(t, deliver(absorbed), "a fresh label while run-1 serves", "run-1")
		runs.end("run-1")
		if w := deliver(absorbed, "d-9"); w.Code != http.StatusOK || laneResp(t, w)["status"] != webhooks.StatusDuplicate {
			t.Fatalf("the absorbed event back with an id: code=%d body=%s, want 200 duplicate", w.Code, w.Body.String())
		}
		if n := runs.count(); n != 1 {
			t.Fatalf("launches = %d, want 1", n)
		}
	})
	t.Run("board", func(t *testing.T) {
		s, board, _ := newBoardLaneServer(t)
		deliver := f.bind(t, s, "")
		first := f.labeled("2026-11-18T10:00:00Z", "implement")
		c1 := wantCarded(t, deliver(first), "first label", http.StatusAccepted)
		finishCard(t, board, c1)
		wantCarded(t, deliver(first, "d-9"), "the carded event back with an id", http.StatusOK)
		if n := len(laneCards(t, board)); n != 1 {
			t.Fatalf("cards = %d, want 1", n)
		}
	})
}

// Promise 3, direct lane: a fresh label while the run still serves is
// absorbed — no concurrent run — and stays a duplicate even redelivered
// after the run has ended. A fresh label after that launches.
func TestTriggerLane_Direct_AFreshLabelWhileTheRunServesIsAbsorbed(t *testing.T) {
	for _, f := range laneForges() {
		t.Run(f.name, func(t *testing.T) {
			s, runs, counter := newDirectLaneServer(t)
			deliver := f.bind(t, s, "")
			wantLaunched(t, deliver(f.labeled("2026-11-18T10:00:00Z", "implement")), "first label")
			absorbed := f.labeled("2026-11-18T10:05:00Z", "implement")
			wantDuplicate(t, deliver(absorbed), "a fresh label while run-1 serves", "run-1")
			if n := runs.count(); n != 1 {
				t.Fatalf("launches = %d while run-1 serves, want 1", n)
			}
			runs.end("run-1")
			w := deliver(absorbed)
			if w.Code != http.StatusOK || laneResp(t, w)["status"] != webhooks.StatusDuplicate {
				t.Fatalf("redelivery of the absorbed label: code=%d body=%s, want 200 duplicate", w.Code, w.Body.String())
			}
			if n := runs.count(); n != 1 {
				t.Fatalf("launches = %d, want 1 — the absorbed request was served by run-1", n)
			}
			wantLaunched(t, deliver(f.labeled("2026-11-18T11:00:00Z", "implement")), "a fresh label after run-1 ended")
			if got := monthlyRuns(t, counter); got != 2 {
				t.Fatalf("monthly runs = %d, want 2", got)
			}
		})
	}
}

// Promise 3, under overlap: supersede the next claim replaces the serving
// run — the policy the launch tail applies to every other lane.
func TestTriggerLane_Direct_SupersedeReplacesTheServingRun(t *testing.T) {
	f := laneForges()[0]
	s, runs, _ := newDirectLaneServer(t)
	deliver := f.bind(t, s, "supersede")
	wantLaunched(t, deliver(f.labeled("2026-11-18T10:00:00Z", "implement")), "first label")
	wantLaunched(t, deliver(f.labeled("2026-11-18T10:05:00Z", "implement")), "a fresh label under supersede")
	if n := runs.count(); n != 2 {
		t.Fatalf("launches = %d, want 2", n)
	}
	if !slices.Equal(runs.canceled, []string{"run-1"}) {
		t.Fatalf("canceled = %v, want [run-1] — the newest request wins", runs.canceled)
	}
}

// barrierDeliveries makes the first two reads of one idempotency key meet
// before either returns: both deliveries see the key free, neither has
// inserted yet — the race the lane's unique next key must decide.
type barrierDeliveries struct {
	webhooks.DeliveryStore
	key     string
	arrived sync.WaitGroup
	mu      sync.Mutex
	armed   int
}

func (b *barrierDeliveries) GetByIdempotencyKey(ctx context.Context, key string) (webhooks.Delivery, error) {
	d, err := b.DeliveryStore.GetByIdempotencyKey(ctx, key)
	if key == b.key {
		b.mu.Lock()
		wait := b.armed > 0
		if wait {
			b.armed--
		}
		b.mu.Unlock()
		if wait {
			b.arrived.Done()
			b.arrived.Wait()
		}
	}
	return d, err
}

// Promise 3, racing: two fresh labels reaching a free lane together claim it
// once — one launch, one quota unit — in either order of arrival, and when
// both read the free lane before either writes.
func TestTriggerLane_Direct_TwoFreshLabelsRacingForAFreeLaneLaunchOnce(t *testing.T) {
	f := laneForges()[0]
	second, third := f.labeled("2026-11-18T11:00:00Z", "implement"), f.labeled("2026-11-18T11:00:01Z", "implement")
	for name, order := range map[string][2]string{"in order": {second, third}, "reversed": {third, second}} {
		t.Run(name, func(t *testing.T) {
			s, runs, counter := newDirectLaneServer(t)
			deliver := f.bind(t, s, "")
			wantLaunched(t, deliver(f.labeled("2026-11-18T10:00:00Z", "implement")), "first label")
			runs.end("run-1")
			wantLaunched(t, deliver(order[0]), "the first fresh label")
			wantDuplicate(t, deliver(order[1]), "the second fresh label", "run-2")
			if n, got := runs.count(), monthlyRuns(t, counter); n != 2 || got != 2 {
				t.Fatalf("launches = %d, monthly runs = %d, want 2 and 2", n, got)
			}
		})
	}
	t.Run("concurrent", func(t *testing.T) {
		s, runs, counter := newDirectLaneServer(t)
		deliver := f.bind(t, s, "")
		wantLaunched(t, deliver(f.labeled("2026-11-18T10:00:00Z", "implement")), "first label")
		runs.end("run-1")
		rows, err := s.webhookDeliveries.ListByWebhook(context.Background(), "t1", "ghw", 10)
		if err != nil || len(rows) != 1 {
			t.Fatalf("ListByWebhook = %v, %v; want the one claim", rows, err)
		}
		b := &barrierDeliveries{DeliveryStore: s.webhookDeliveries, key: laneGenerationKey(rows[0].IdempotencyKey, 1), armed: 2}
		b.arrived.Add(2)
		s.webhookDeliveries = b
		var wg sync.WaitGroup
		bodies := []string{second, third}
		codes := make([]int, 2)
		for i, body := range bodies {
			wg.Add(1)
			go func(i int, body string) {
				defer wg.Done()
				codes[i] = deliver(body).Code
			}(i, body)
		}
		wg.Wait()
		loser := slices.Index(codes, http.StatusOK)
		if sorted := slices.Sorted(slices.Values(codes)); !slices.Equal(sorted, []int{http.StatusOK, http.StatusAccepted}) || loser < 0 {
			t.Fatalf("codes = %v, want one 202 launch and one 200 duplicate", codes)
		}
		if n, got := runs.count(), monthlyRuns(t, counter); n != 2 || got != 2 {
			t.Fatalf("launches = %d, monthly runs = %d, want 2 and 2 — the loser's quota unit is handed back", n, got)
		}
		// The loser was absorbed by the winner's claim: its resend once that
		// run ended stays a duplicate.
		runs.end("run-2")
		if w := deliver(bodies[loser]); w.Code != http.StatusOK || laneResp(t, w)["status"] != webhooks.StatusDuplicate {
			t.Fatalf("the race loser resent after run-2 ended: code=%d body=%s, want 200 duplicate", w.Code, w.Body.String())
		}
		if n := runs.count(); n != 2 {
			t.Fatalf("launches = %d, want 2", n)
		}
	})
}

// The claim a fresh label reaches decides it by its launch verdict when it
// names no run — with the launch tail's own retry predicates, so a failed
// launch is retried by the next label and a spent one frees the lane.
func TestTriggerLane_Direct_AClaimWithNoRunIsJudgedByItsLaunchVerdict(t *testing.T) {
	f := laneForges()[0]
	base := knowledge.ChecksumHex([]byte("gh|issue|t1|ghw|acme/widgets|42|implement"))
	for _, tc := range []struct {
		name   string
		row    webhooks.Delivery
		served bool // the fresh label is absorbed instead of launching
	}{
		{name: "a launch still in flight absorbs it", row: webhooks.Delivery{Status: webhooks.StatusAccepted}, served: true},
		{name: "an uncertain launch absorbs it", row: webhooks.Delivery{Status: webhooks.StatusLaunchUncertain}, served: true},
		{name: "a failed launch is retried by it", row: webhooks.Delivery{Status: webhooks.StatusLaunchError, Attempts: 1}},
		{name: "a spent retry frees the lane", row: webhooks.Delivery{Status: webhooks.StatusLaunchError, Attempts: webhookMaxAttempts(),
			RetryTarget: &webhooks.DeferredTarget{BotID: "feature-dev", IdemKey: base}}},
		{name: "an exhausted launch frees the lane", row: webhooks.Delivery{Status: webhooks.StatusLaunchExhausted, Attempts: 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, runs, _ := newDirectLaneServer(t)
			deliver := f.bind(t, s, "")
			row := tc.row
			row.ID, row.TenantID, row.WebhookID, row.Provider = "prior", "t1", "ghw", webhooks.ProviderGitHub
			row.IdempotencyKey, row.BotID, row.SubjectID, row.ProjectPath = base, "feature-dev", "issue:42", "acme/widgets"
			row.PayloadHash, row.ReceivedAt = "an-earlier-event", time.Now().UTC().Add(-time.Hour)
			if err := s.webhookDeliveries.Insert(context.Background(), row); err != nil {
				t.Fatal(err)
			}
			w := deliver(f.labeled("2026-11-18T11:00:00Z", "implement"))
			if tc.served {
				if w.Code != http.StatusOK || laneResp(t, w)["status"] != webhooks.StatusDuplicate || runs.count() != 0 {
					t.Fatalf("code=%d body=%s launches=%d, want an absorbed duplicate and no launch", w.Code, w.Body.String(), runs.count())
				}
				return
			}
			wantLaunched(t, w, "a fresh label")
			if n := runs.count(); n != 1 {
				t.Fatalf("launches = %d, want 1", n)
			}
		})
	}
}

// webhookRunActive reads the run itself: any status but a terminal one
// serves, a run the store does not hold serves nothing, and no run store is
// an error rather than a guess.
func TestWebhookRunActiveReadsTheRunsOwnStatus(t *testing.T) {
	s := newWebhookTestServer(t)
	dir := t.TempDir()
	s.runs = newTestRunviewService(t, dir)
	rs, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	for id, st := range map[string]store.RunStatus{
		"r-running": store.RunStatusRunning, "r-paused": store.RunStatusPausedWaitingHuman,
		"r-finished": store.RunStatusFinished, "r-resumable": store.RunStatusFailedResumable,
	} {
		finishRunAs(t, rs, id, st)
	}
	for id, want := range map[string]bool{"r-running": true, "r-paused": true, "r-finished": false, "r-resumable": false, "r-missing": false} {
		got, err := s.webhookRunActive(context.Background(), id)
		if err != nil || got != want {
			t.Fatalf("webhookRunActive(%s) = %v, %v; want %v, nil", id, got, err, want)
		}
	}
	s.runs = nil
	if _, err := s.webhookRunActive(context.Background(), "r-running"); err == nil {
		t.Fatal("no run store answered without an error")
	}
}

// failingDeliveries answers every idempotency read with a store failure.
type failingDeliveries struct{ webhooks.DeliveryStore }

func (failingDeliveries) GetByIdempotencyKey(context.Context, string) (webhooks.Delivery, error) {
	return webhooks.Delivery{}, errors.New("mongo: server selection timeout")
}

// failingFirstRead fails only the first idempotency read — the
// absorbed-delivery lookup every lane delivery starts with.
type failingFirstRead struct {
	webhooks.DeliveryStore
	failed bool
}

func (f *failingFirstRead) GetByIdempotencyKey(ctx context.Context, key string) (webhooks.Delivery, error) {
	if !f.failed {
		f.failed = true
		return webhooks.Delivery{}, errors.New("mongo: server selection timeout")
	}
	return f.DeliveryStore.GetByIdempotencyKey(ctx, key)
}

// failingKey fails the reads of one idempotency key only.
type failingKey struct {
	webhooks.DeliveryStore
	key string
}

func (f failingKey) GetByIdempotencyKey(ctx context.Context, key string) (webhooks.Delivery, error) {
	if key == f.key {
		return webhooks.Delivery{}, errors.New("mongo: server selection timeout")
	}
	return f.DeliveryStore.GetByIdempotencyKey(ctx, key)
}

// failingBoard answers every listing with a store failure.
type failingBoard struct{ native.BoardStore }

func (failingBoard) List(native.ListFilter) ([]*native.Issue, error) {
	return nil, errors.New("mongo: server selection timeout")
}

// A lane the server cannot read refuses the delivery as retryable — never a
// guess in either direction, and nothing metered.
func TestTriggerLane_AnUnreadableLaneRefusesTheDeliveryRetryably(t *testing.T) {
	f := laneForges()[0]
	t.Run("direct", func(t *testing.T) {
		s, runs, counter := newDirectLaneServer(t)
		deliver := f.bind(t, s, "")
		mem := s.webhookDeliveries
		s.webhookDeliveries = failingDeliveries{mem}
		w := deliver(f.labeled("2026-11-18T10:00:00Z", "implement"))
		if w.Code != http.StatusServiceUnavailable || runs.count() != 0 || monthlyRuns(t, counter) != 0 {
			t.Fatalf("code=%d body=%s launches=%d, want 503 and nothing launched or metered", w.Code, w.Body.String(), runs.count())
		}
		rows, err := mem.ListByWebhook(context.Background(), "t1", "ghw", 10)
		if err != nil || len(rows) != 1 || rows[0].Status != webhooks.StatusLaunchError || !strings.Contains(rows[0].Error, "trigger lane unavailable") {
			t.Fatalf("audit rows = %+v, %v; want one launch_error naming the lane", rows, err)
		}
	})
	t.Run("direct, the absorbed-row read alone", func(t *testing.T) {
		s, runs, counter := newDirectLaneServer(t)
		deliver := f.bind(t, s, "")
		s.webhookDeliveries = &failingFirstRead{DeliveryStore: s.webhookDeliveries}
		w := deliver(f.labeled("2026-11-18T10:00:00Z", "implement"))
		if w.Code != http.StatusServiceUnavailable || runs.count() != 0 || monthlyRuns(t, counter) != 0 {
			t.Fatalf("code=%d body=%s launches=%d, want 503 and nothing launched or metered", w.Code, w.Body.String(), runs.count())
		}
	})
	t.Run("direct, a claim read alone", func(t *testing.T) {
		s, runs, counter := newDirectLaneServer(t)
		deliver := f.bind(t, s, "")
		base := knowledge.ChecksumHex([]byte("gh|issue|t1|ghw|acme/widgets|42|implement"))
		s.webhookDeliveries = failingKey{DeliveryStore: s.webhookDeliveries, key: base}
		w := deliver(f.labeled("2026-11-18T10:00:00Z", "implement"))
		if w.Code != http.StatusServiceUnavailable || runs.count() != 0 || monthlyRuns(t, counter) != 0 {
			t.Fatalf("code=%d body=%s launches=%d, want 503 and nothing launched or metered", w.Code, w.Body.String(), runs.count())
		}
	})
	t.Run("board", func(t *testing.T) {
		s, board, counter := newBoardLaneServer(t)
		deliver := f.bind(t, s, "")
		s.cfg.CloudBoardFor = func(string) native.BoardStore { return failingBoard{board} }
		w := deliver(f.labeled("2026-11-18T10:00:00Z", "implement"))
		if w.Code != http.StatusServiceUnavailable || len(laneCards(t, board)) != 0 || monthlyRuns(t, counter) != 0 {
			t.Fatalf("code=%d body=%s, want 503 and no card", w.Code, w.Body.String())
		}
	})
}

// Promise 4, direct lane: a comment command is claimed once for good. The
// same comment delivered again with different bytes, after its run ended,
// launches nothing — a comment is its own subject, not a re-armable trigger.
func TestTriggerLane_ACommentCommandIsClaimedOnce(t *testing.T) {
	s, runs, _ := newDirectLaneServer(t)
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"review-pr", "feature-dev"}
	cfg.CommandMap = map[string][]webhooks.CommandRoute{
		"featurly": {{BotID: "feature-dev", Mode: "board", ArgsVar: "feature_prompt", Scope: "any"}},
	}
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
		return gateAuthorized, "authorized", nil
	}
	s.webhookPRForgePRResolver = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (forge.PullRef, error) {
		return forge.PullRef{Number: 7, State: "open", HeadRepoFullName: "acme/widgets", SourceBranch: "feat/export", TargetBranch: "main", Author: "alice"}, nil
	}
	deliver := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderIssueComment, pt))
		return w
	}
	wantLaunched(t, deliver(ghIssueCommentFeaturly), "the command")
	runs.end("run-1")
	again := strings.Replace(ghIssueCommentFeaturly, `"comment": {"id": 555,`, `"comment": {"id": 555, "updated_at": "2026-11-18T11:00:00Z",`, 1)
	wantDuplicate(t, deliver(again), "the same comment, other bytes, after its run ended", "run-1")
	if n := runs.count(); n != 1 {
		t.Fatalf("launches = %d, want 1", n)
	}
}

// --- the board lane ---

// barrierBoard makes the first two List calls meet before either returns: both
// deliveries read the same free lane.
type barrierBoard struct {
	native.BoardStore
	arrived sync.WaitGroup
	mu      sync.Mutex
	armed   int
}

func (b *barrierBoard) List(f native.ListFilter) ([]*native.Issue, error) {
	b.mu.Lock()
	wait := b.armed > 0
	if wait {
		b.armed--
	}
	b.mu.Unlock()
	out, err := b.BoardStore.List(f)
	if wait {
		b.arrived.Done()
		b.arrived.Wait()
	}
	return out, err
}

func newBoardLaneServer(t *testing.T) (*Server, native.BoardStore, *orgusage.MemoryCounter) {
	t.Helper()
	s := newWebhookTestServer(t)
	counter := orgusage.NewMemoryCounter()
	s.orgUsage = counter
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
	// Non-nil selects the dispatcher branch; the handler never dereferences it.
	s.cfg.CloudBoardCoordinator = &boardmongo.Coordinator{}
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("with a board dispatcher the card is the launch: nothing launches directly")
		return "", nil
	}
	return s, board, counter
}

func laneCards(t *testing.T, board native.BoardStore) []*native.Issue {
	t.Helper()
	cards, err := board.List(native.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return cards
}

func wantCarded(t *testing.T, w *httptest.ResponseRecorder, what string, code int) string {
	t.Helper()
	resp := laneResp(t, w)
	if w.Code != code || resp["status"] != "carded" {
		t.Fatalf("%s: code=%d body=%s, want %d carded", what, w.Code, w.Body.String(), code)
	}
	return resp["card_id"]
}

func finishCard(t *testing.T, board native.BoardStore, id string) {
	t.Helper()
	if _, err := board.SetState(id, native.StateDone); err != nil {
		t.Fatalf("move %s to done: %v", id, err)
	}
}

// Promise 1, board lane: a redelivery of the carded event, once its card is
// done, cards nothing.
func TestTriggerLane_Board_ARedeliveryAfterTheCardEndedCardsNothing(t *testing.T) {
	for _, f := range laneForges() {
		t.Run(f.name, func(t *testing.T) {
			s, board, counter := newBoardLaneServer(t)
			deliver := f.bind(t, s, "")
			first := f.labeled("2026-11-18T10:00:00Z", "implement")
			c1 := wantCarded(t, deliver(first), "first label", http.StatusAccepted)
			cards := laneCards(t, board)
			if len(cards) != 1 || cards[0].ID != c1 || cards[0].State != native.StateReady {
				t.Fatalf("cards = %+v, want one ready card %s", cards, c1)
			}
			finishCard(t, board, c1)
			wantCarded(t, deliver(first), "redelivery after the card ended", http.StatusOK)
			if n := len(laneCards(t, board)); n != 1 {
				t.Fatalf("cards = %d, want 1 — a redelivery is the same request", n)
			}
			if got := monthlyRuns(t, counter); got != 0 {
				t.Fatalf("monthly runs = %d, want 0 — the card pre-check hands its unit back, the dispatcher meters", got)
			}
		})
	}
}

// Promise 2, board lane: re-applying the label once the card is done cards
// the trigger again.
func TestTriggerLane_Board_AFreshLabelAfterTheCardEndedCardsAgain(t *testing.T) {
	for _, f := range laneForges() {
		t.Run(f.name, func(t *testing.T) {
			s, board, _ := newBoardLaneServer(t)
			deliver := f.bind(t, s, "")
			c1 := wantCarded(t, deliver(f.labeled("2026-11-18T10:00:00Z", "implement")), "first label", http.StatusAccepted)
			finishCard(t, board, c1)
			c2 := wantCarded(t, deliver(f.labeled("2026-11-18T11:00:00Z", "implement")), "the label re-applied", http.StatusAccepted)
			// A card given up on (blocked) has ended too.
			if _, err := board.SetState(c2, native.StateBlocked); err != nil {
				t.Fatal(err)
			}
			c3 := wantCarded(t, deliver(f.labeled("2026-11-18T12:00:00Z", "implement")), "the label re-applied after a blocked card", http.StatusAccepted)
			if c3 == "" || c3 == c2 || c3 == c1 {
				t.Fatalf("third card id = %q, want a new card beside %s and %s", c3, c1, c2)
			}
			if c2 == "" || c2 == c1 {
				t.Fatalf("second card id = %q, want a new card beside %s", c2, c1)
			}
			cards := laneCards(t, board)
			if len(cards) != 3 {
				t.Fatalf("cards = %d, want 3", len(cards))
			}
			for _, c := range cards {
				if c.ID == c3 && c.State != native.StateReady {
					t.Fatalf("the new card is %q, want ready for the dispatcher", c.State)
				}
			}
		})
	}
}

// Promise 3, board lane: a fresh label while the card still serves cards
// nothing, and stays a duplicate redelivered after the card is done.
func TestTriggerLane_Board_AFreshLabelWhileTheCardServesIsAbsorbed(t *testing.T) {
	for _, f := range laneForges() {
		t.Run(f.name, func(t *testing.T) {
			s, board, _ := newBoardLaneServer(t)
			deliver := f.bind(t, s, "")
			c1 := wantCarded(t, deliver(f.labeled("2026-11-18T10:00:00Z", "implement")), "first label", http.StatusAccepted)
			if _, err := board.SetState(c1, native.StateInProgress); err != nil {
				t.Fatal(err)
			}
			absorbed := f.labeled("2026-11-18T10:05:00Z", "implement")
			if got := wantCarded(t, deliver(absorbed), "a fresh label while the card serves", http.StatusOK); got != c1 {
				t.Fatalf("answered by card %q, want the serving card %s", got, c1)
			}
			if n := len(laneCards(t, board)); n != 1 {
				t.Fatalf("cards = %d while the card serves, want 1", n)
			}
			finishCard(t, board, c1)
			wantCarded(t, deliver(absorbed), "redelivery of the absorbed label", http.StatusOK)
			if n := len(laneCards(t, board)); n != 1 {
				t.Fatalf("cards = %d, want 1 — the absorbed request was served by %s", n, c1)
			}
			wantCarded(t, deliver(f.labeled("2026-11-18T11:00:00Z", "implement")), "a fresh label after the card ended", http.StatusAccepted)
			if n := len(laneCards(t, board)); n != 2 {
				t.Fatalf("cards = %d, want 2", n)
			}
		})
	}
}

// Promise 3, racing on the board: two fresh labels that both read the free
// lane before either cards it collide on the lane's next id — one card.
func TestTriggerLane_Board_TwoFreshLabelsRacingForAFreeLaneCardOnce(t *testing.T) {
	f := laneForges()[0]
	s, board, _ := newBoardLaneServer(t)
	deliver := f.bind(t, s, "")
	c1 := wantCarded(t, deliver(f.labeled("2026-11-18T10:00:00Z", "implement")), "first label", http.StatusAccepted)
	finishCard(t, board, c1)
	b := &barrierBoard{BoardStore: board, armed: 2}
	b.arrived.Add(2)
	s.cfg.CloudBoardFor = func(string) native.BoardStore { return b }
	var wg sync.WaitGroup
	codes := make([]int, 2)
	racers := []string{f.labeled("2026-11-18T11:00:00Z", "implement"), f.labeled("2026-11-18T11:00:01Z", "implement")}
	for i, body := range racers {
		wg.Add(1)
		go func(i int, body string) {
			defer wg.Done()
			codes[i] = deliver(body).Code
		}(i, body)
	}
	wg.Wait()
	if sorted := slices.Sorted(slices.Values(codes)); !slices.Equal(sorted, []int{http.StatusOK, http.StatusAccepted}) {
		t.Fatalf("codes = %v, want one 202 card and one 200 duplicate", codes)
	}
	if n := len(laneCards(t, board)); n != 2 {
		t.Fatalf("cards = %d, want 2 — the done card and ONE new one", n)
	}
	// The loser was absorbed by the winner's card: its resend once that card
	// is done cards nothing.
	loser := slices.Index(codes, http.StatusOK)
	for _, c := range laneCards(t, b) {
		if c.State != native.StateDone {
			finishCard(t, board, c.ID)
		}
	}
	wantCarded(t, deliver(racers[loser]), "the race loser resent after the winner's card ended", http.StatusOK)
	if n := len(laneCards(t, board)); n != 2 {
		t.Fatalf("cards = %d after the loser's resend, want 2", n)
	}
}

// The lane is (repository, issue, label): issue 42 of another repository, or
// another label on the same issue, is its own lane — a card serving one
// blocks neither.
func TestTriggerLane_Board_TheLaneIsTheRepositoryTheIssueAndTheLabel(t *testing.T) {
	f := laneForges()[0]
	s, board, _ := newBoardLaneServer(t)
	deliver := f.bind(t, s, "")
	wantCarded(t, deliver(f.labeled("2026-11-18T10:00:00Z", "implement")), "first label", http.StatusAccepted)
	otherRepo := strings.NewReplacer(`"full_name": "acme/widgets"`, `"full_name": "acme/gadgets"`,
		`https://github.com/acme/widgets`, `https://github.com/acme/gadgets`).Replace(f.labeled("2026-11-18T10:01:00Z", "implement"))
	wantCarded(t, deliver(otherRepo), "the same issue number in another repository", http.StatusAccepted)
	wantCarded(t, deliver(f.labeled("2026-11-18T10:02:00Z", "bug")), "another label on the same issue", http.StatusAccepted)
	if n := len(laneCards(t, board)); n != 3 {
		t.Fatalf("cards = %d, want 3 — three lanes", n)
	}
}

// A card carded before lanes existed carries only the subject label: it
// still blocks its subject while it serves, and frees it once done.
func TestTriggerLane_Board_ACardFromBeforeLanesServesItsSubject(t *testing.T) {
	f := laneForges()[0]
	s, board, _ := newBoardLaneServer(t)
	deliver := f.bind(t, s, "")
	legacy, err := board.Create(native.Issue{
		Title: "feature-dev — Add a CSV export endpoint", State: native.StateReady, Bot: "feature-dev",
		Labels:  []string{"cmd:issue:42", "source:command", "provider:github"},
		BotArgs: map[string]string{boardProjectPathKey: "acme/widgets"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := wantCarded(t, deliver(f.labeled("2026-11-18T10:00:00Z", "implement")), "a label while the old card serves", http.StatusOK); got != legacy.ID {
		t.Fatalf("answered by %q, want the old card %s", got, legacy.ID)
	}
	finishCard(t, board, legacy.ID)
	wantCarded(t, deliver(f.labeled("2026-11-18T11:00:00Z", "implement")), "a label once the old card ended", http.StatusAccepted)
	if n := len(laneCards(t, board)); n != 2 {
		t.Fatalf("cards = %d, want 2", n)
	}
}

// Promise 4, board lane: a comment command's card answers its comment for
// good, whatever bytes a later delivery of that comment carries.
func TestTriggerLane_Board_ACommentCommandCardsOnce(t *testing.T) {
	s, board, _ := newBoardLaneServer(t)
	cfg, pt := ghConfig(t, s)
	cfg.BotIDs = []string{"review-pr", "feature-dev"}
	cfg.CommandMap = map[string][]webhooks.CommandRoute{
		"featurly": {{BotID: "feature-dev", Mode: "board", ArgsVar: "feature_prompt", Scope: "any"}},
	}
	s.webhookPRForgeCommandGate = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (prforgeGateOutcome, string, error) {
		return gateAuthorized, "authorized", nil
	}
	s.webhookPRForgePRResolver = func(context.Context, webhooks.Config, webhooks.Provider, prforge.ParsedNote, webhooks.CommandRoute) (forge.PullRef, error) {
		return forge.PullRef{Number: 7, State: "open", HeadRepoFullName: "acme/widgets", SourceBranch: "feat/export", TargetBranch: "main", Author: "alice"}, nil
	}
	deliver := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderIssueComment, pt))
		return w
	}
	wantCarded(t, deliver(ghIssueCommentFeaturly), "the command", http.StatusAccepted)
	cards := laneCards(t, board)
	if len(cards) != 1 {
		t.Fatalf("cards = %d, want 1", len(cards))
	}
	finishCard(t, board, cards[0].ID)
	again := strings.Replace(ghIssueCommentFeaturly, `"comment": {"id": 555,`, `"comment": {"id": 555, "updated_at": "2026-11-18T11:00:00Z",`, 1)
	wantCarded(t, deliver(again), "the same comment, other bytes, after its card ended", http.StatusOK)
	if n := len(laneCards(t, board)); n != 1 {
		t.Fatalf("cards = %d, want 1", n)
	}
}
