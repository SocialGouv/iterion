package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/binding"
	"github.com/SocialGouv/iterion/pkg/connector/exec"
	"github.com/SocialGouv/iterion/pkg/connector/intake"
	"github.com/SocialGouv/iterion/pkg/connector/poller"
	"github.com/SocialGouv/iterion/pkg/connector/polling"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// feedVendor answers a search under its query q with the items it holds,
// but for those it lags on under that query: an index one query reads late.
type feedVendor struct {
	mu      sync.Mutex
	items   []map[string]any
	lagging map[string]map[string]bool
	srv     *httptest.Server
}

func newFeedVendor(t *testing.T) *feedVendor {
	v := &feedVendor{lagging: map[string]map[string]bool{}}
	v.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		defer v.mu.Unlock()
		if r.URL.Path != "/search" || r.Header.Get("Authorization") != "Bearer vendor-token" {
			http.Error(w, "no such resource", http.StatusNotFound)
			return
		}
		q := r.URL.Query().Get("q")
		out := []map[string]any{}
		for _, it := range v.items {
			if !v.lagging[q][it["id"].(string)] {
				out = append(out, it)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": out})
	}))
	t.Cleanup(v.srv.Close)
	return v
}

// put stores an item at a revision, replacing its earlier one.
func (v *feedVendor) put(id, rev, title string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	it := map[string]any{"id": id, "rev": rev, "title": title}
	for i, cur := range v.items {
		if cur["id"] == id {
			v.items[i] = it
			return
		}
	}
	v.items = append(v.items, it)
}

func (v *feedVendor) lag(q, id string, on bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.lagging[q] == nil {
		v.lagging[q] = map[string]bool{}
	}
	v.lagging[q][id] = on
}

func polledFeedPackage() *spec.Package {
	return &spec.Package{
		Connector: spec.Connector{
			SchemaVersion: spec.SchemaVersion, ID: "feed", Version: "1.0.0",
			BaseURL:         spec.BaseURL{OperatorSupplied: true},
			Auth:            []spec.AuthScheme{{ID: "token", Kind: spec.AuthBearer}},
			DefaultSecurity: []spec.SecurityRequirement{{Terms: []spec.SecurityTerm{{SchemeID: "token"}}}},
			Maturity:        spec.MaturityExperimental,
		},
		Ops: []spec.OpsFile{{SchemaVersion: spec.SchemaVersion, Connector: "feed", Domain: "item", Operations: []spec.Operation{{
			ID: "feed.item.search", Resource: "item", Verb: "search",
			HTTP:    spec.HTTPBinding{Method: "GET", Path: "/search"},
			Params:  []spec.Param{{Key: "q", Name: "q", In: spec.InQuery, Type: "string", Required: true}},
			Results: []spec.ResultCase{{Status: 200}}, Effect: spec.EffectRead, Deterministic: true,
		}}}},
		Triggers: []spec.Trigger{{
			ID: "feed.item.changed", Delivery: spec.DeliveryPoll,
			Poll: &spec.TriggerPoll{
				Operation: "feed.item.search", Params: map[string]string{"q": "query.q"},
				Query: []spec.PollQueryField{{ID: "q", Required: true}},
				Items: "response.items", ItemID: "item.id", ItemVersion: "item.rev",
				Interval: spec.PollInterval{Default: "5m", Min: "1m"},
			},
			Outputs: []spec.TriggerOutput{{Name: "prompt", From: "payload.title"}},
		}},
	}
}

// launchRecorder stands behind the real submitter as the launch path: it
// records the plans that launched, as (subscription, intake), and answers a
// run per submission key; fail, when it says so, fails a plan transiently.
type launchRecorder struct {
	mu       sync.Mutex
	launched []string
	fail     func(trigger.LaunchPlan) error
}

func (l *launchRecorder) Launch(_ context.Context, p trigger.LaunchPlan) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail != nil {
		if err := l.fail(p); err != nil {
			return "", err
		}
	}
	l.launched = append(l.launched, p.SourceRef.ScheduleID+" "+p.SourceRef.IntakeID)
	return "run-" + p.SubmissionKey, nil
}

func (l *launchRecorder) runs() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := slices.Clone(l.launched)
	slices.Sort(out)
	return out
}

// polledDeliveryWorld is one tenant's feed connection with two pollers of
// one trigger — queries A and B — and the server's delivery chain behind
// them: the poller tick, the activation, the matcher, the intake worker and
// the connector submitter, on one clock.
type polledDeliveryWorld struct {
	t        *testing.T
	s        *Server
	vendor   *feedVendor
	pollers  *poller.MemoryStore
	intakes  *intake.MemoryStore
	subs     *trigger.MemorySubscriptionStore
	a, b     poller.Poller
	ticker   *polling.Worker
	worker   *intake.Worker
	launches *launchRecorder
	clock    *pairClock
}

const queryA, queryB = "project = A", "project = B"

func newPolledDeliveryWorld(t *testing.T) *polledDeliveryWorld {
	t.Helper()
	ctx := context.Background()
	sealer, err := secrets.NewAESGCMSealer([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	w := &polledDeliveryWorld{
		t: t, vendor: newFeedVendor(t), pollers: poller.NewMemoryStore(), intakes: intake.NewMemoryStore(),
		subs: trigger.NewMemorySubscriptionStore(), launches: &launchRecorder{},
		clock: &pairClock{now: time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)},
	}
	conns := connection.NewMemoryStore()
	sealed, err := connection.SealToken(sealer, "conn-feed", "vendor-token", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if err := conns.Create(ctx, connection.Connection{
		ID: "conn-feed", TenantID: "t1", Connector: "feed", Alias: "main", SchemeID: "token",
		Status: connection.StatusActive, Capabilities: []connection.Capability{connection.CapAction, connection.CapTrigger},
		BaseURL: w.vendor.srv.URL, AuthPlacement: connection.AuthPlacement{Kind: "bearer"}, SealedPayload: sealed,
	}); err != nil {
		t.Fatal(err)
	}
	cat := connection.NewMemoryCatalog(polledFeedPackage())
	catalog := func(string) connection.Catalog { return cat }
	w.s = newOrgTestServer(t)
	w.s.connectorPollers, w.s.connectorIntakes, w.s.cfg.TriggerStore = w.pollers, w.intakes, w.subs
	w.s.connectionStore, w.s.connectorCatalog, w.s.sealer = conns, cat, sealer
	for _, q := range []string{queryA, queryB} {
		p, _, err := w.pollers.Ensure(ctx, poller.Poller{TenantID: "t1", ConnectionID: "conn-feed", TriggerID: "feed.item.changed", Query: map[string]string{"q": q}, CreatedAt: w.clock.now})
		if err != nil {
			t.Fatal(err)
		}
		if q == queryA {
			w.a = p
		} else {
			w.b = p
		}
	}
	for id, p := range map[string]poller.Poller{"s1": w.a, "s-b": w.b} {
		if err := w.subs.Create(ctx, trigger.Subscription{
			ID: id, TenantID: "t1", BotID: "bot-" + id, Enabled: true, Invocation: bundle.InvocationKindConnector,
			ConnectionID: "conn-feed", TriggerID: "feed.item.changed", Poller: p.ID, PollQuery: p.Query,
		}); err != nil {
			t.Fatal(err)
		}
	}
	w.ticker = &polling.Worker{
		Pollers: w.pollers, Intakes: w.intakes, Subscriptions: w.subs, Catalog: catalog,
		Resolver: func(tenant string) *connection.Resolver {
			return &connection.Resolver{Catalog: cat, Store: conns, Sealer: sealer, TenantID: tenant}
		},
		// The vendor double is loopback, which the guarded dialer refuses on
		// purpose; MarkGuarded declares that one exemption.
		Client: exec.MarkGuarded(w.vendor.srv.Client()), Owner: "poller",
		Policy: polling.Policy{SeenRetention: polling.DefaultSeenRetention, IntakeRetention: 30 * 24 * time.Hour},
		Now:    w.clock.Now,
	}
	matcher := &binding.Matcher{Subscriptions: w.subs, Catalog: catalog}
	pace := intake.Policy{MaxAttempts: 5, Backoff: time.Minute, BackoffMax: 10 * time.Minute, ClaimTTL: time.Minute}
	w.worker = &intake.Worker{
		Store: w.intakes, Matcher: matcher, Owner: "intake-worker", Intakes: pace, Pairs: pace, Now: w.clock.Now,
		Submitter: &connectorSubmitter{
			subs: w.subs, matcher: matcher, launcher: w.launches,
			botVars:     func(context.Context, string, string) ([]string, bool, error) { return []string{"prompt"}, true, nil },
			repoTrusted: func(context.Context, intake.Intake, string) error { return nil },
		},
	}
	w.tick(w.a)
	w.tick(w.b) // both primed, empty
	return w
}

// tick runs one scan of poller p as it is stored now.
func (w *polledDeliveryWorld) tick(p poller.Poller) {
	w.t.Helper()
	cur, err := w.pollers.Get(context.Background(), "t1", p.ID)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := w.ticker.Tick(context.Background(), cur); err != nil {
		w.t.Fatalf("tick %s: %v", p.ID, err)
	}
}

func (w *polledDeliveryWorld) match() {
	w.t.Helper()
	if _, err := w.worker.MatchTick(context.Background()); err != nil {
		w.t.Fatal(err)
	}
}

func (w *polledDeliveryWorld) launch() {
	w.t.Helper()
	if _, err := w.worker.LaunchTick(context.Background()); err != nil {
		w.t.Fatal(err)
	}
}

// move re-binds s1 to poller p's query through the server's activation.
func (w *polledDeliveryWorld) move(p poller.Poller) {
	w.t.Helper()
	ctx := context.Background()
	cur, err := w.subs.Get(ctx, "s1")
	if err != nil {
		w.t.Fatal(err)
	}
	next := cur
	next.Poller, next.PollQuery = p.ID, p.Query
	if status, err := w.s.activatePolledSubscription(ctx, "t1", &cur, &next); err != nil {
		w.t.Fatalf("activation = %d %v", status, err)
	}
	if err := w.subs.Update(ctx, next); err != nil {
		w.t.Fatal(err)
	}
}

// intakeOf is the id of poller p's intake of an item revision.
func intakeOf(p poller.Poller, id, rev string) string {
	return intake.IntakeID("t1", intake.PollerSource(p.ID), polling.DedupKey(poller.ItemKey{ID: id, Version: rev}))
}

func (w *polledDeliveryWorld) pair(intakeID, sub string) intake.Pair {
	w.t.Helper()
	p, err := w.intakes.GetPair(context.Background(), "t1", intake.PairID(intakeID, sub))
	if err != nil {
		w.t.Fatalf("pair of %s for %s: %v", intakeID, sub, err)
	}
	return p
}

// expect checks the launches so far, as (subscription, intake), sorted.
func (w *polledDeliveryWorld) expect(when string, want ...string) {
	w.t.Helper()
	slices.Sort(want)
	if got := w.launches.runs(); !slices.Equal(got, want) {
		w.t.Fatalf("%s: launched %v, want %v", when, got, want)
	}
}

// newRevision is case 3, the end of every scenario: a new revision of the
// item, on the moved subscription's poller, launches once for each of its
// subscriptions.
func (w *polledDeliveryWorld) newRevision(before []string) {
	w.t.Helper()
	w.vendor.put("ENG-1", "2", "edited")
	w.clock.now = w.clock.now.Add(time.Minute)
	w.tick(w.b)
	w.match()
	w.launch()
	w.expect("after a new revision", append(before, "s1 "+intakeOf(w.b, "ENG-1", "2"), "s-b "+intakeOf(w.b, "ENG-1", "2"))...)
}

// A subscription moved to the query of an existing poller that lags does
// not receive again what its first poller delivered: the lagging poller's
// pair of the same revision is refused at launch, naming the pair and run
// that delivered it — whether the activation's catch-up or the matcher
// wrote it.
func TestAMovedSubscriptionIsNotLaunchedARevisionItReceived(t *testing.T) {
	for _, via := range []string{"catch-up", "matcher"} {
		t.Run("via the "+via, func(t *testing.T) {
			w := newPolledDeliveryWorld(t)
			w.vendor.lag(queryB, "ENG-1", true)
			w.vendor.put("ENG-1", "1", "first")
			w.clock.now = w.clock.now.Add(time.Minute)
			w.tick(w.a)
			w.match()
			w.launch()
			fromA := "s1 " + intakeOf(w.a, "ENG-1", "1")
			w.expect("before the move", fromA)
			delivered := w.pair(intakeOf(w.a, "ENG-1", "1"), "s1")

			w.move(w.b)
			if via == "matcher" {
				// The activation settles while B still lags: the matcher
				// alone writes B's pair.
				w.tick(w.b)
				if s, _ := w.subs.Get(context.Background(), "s1"); !s.BackfillDone {
					t.Fatal("the activation did not settle before the item reached its new poller")
				}
			}
			w.vendor.lag(queryB, "ENG-1", false)
			w.clock.now = w.clock.now.Add(time.Minute)
			w.tick(w.b)
			onB := intakeOf(w.b, "ENG-1", "1")
			_, err := w.intakes.GetPair(context.Background(), "t1", intake.PairID(onB, "s1"))
			if wrote := err == nil; wrote != (via == "catch-up") {
				t.Fatalf("B's pair for s1 written by the tick = %v, want %v: the %s is not the writer under test", wrote, via == "catch-up", via)
			}
			w.match()
			w.launch()
			w.expect("after the lagging poller accepted the revision", fromA, "s-b "+onB)
			want := "already delivered to this subscription: pair " + delivered.ID + " launched run " + delivered.RunID + " for the same item revision"
			if p := w.pair(onB, "s1"); p.State != intake.PairRefused || p.Reason != want {
				t.Fatalf("B's pair for s1 = %+v, want refused saying %q", p, want)
			}
			w.newRevision([]string{fromA, "s-b " + onB})
		})
	}
}

// A pair the first poller still owed when the subscription moved delivered
// nothing: the lagging poller's pair of the same revision launches — exactly
// one run, through the new poller — whichever pair the launch judges first.
func TestAMovedSubscriptionIsLaunchedARevisionItsOldPairStillOwed(t *testing.T) {
	for _, newFirst := range []bool{false, true} {
		name := "the old pair judged first"
		if newFirst {
			name = "the new pair judged first"
		}
		t.Run(name, func(t *testing.T) {
			w := newPolledDeliveryWorld(t)
			w.vendor.lag(queryB, "ENG-1", true)
			w.vendor.put("ENG-1", "1", "first")
			w.clock.now = w.clock.now.Add(time.Minute)
			w.tick(w.a)
			w.match()
			onA := intakeOf(w.a, "ENG-1", "1")
			if newFirst {
				// The old pair's first attempt fails before the move: it
				// waits out its backoff while the new one is judged.
				failed := false
				w.launches.fail = func(p trigger.LaunchPlan) error {
					if p.SourceRef.IntakeID == onA && !failed {
						failed = true
						return errors.New("PROBE: the launch path is down")
					}
					return nil
				}
				w.launch()
				if p := w.pair(onA, "s1"); p.State != intake.PairPending || p.Attempts != 1 {
					t.Fatalf("the old pair after its failed attempt = %+v, want pending, one attempt spent", p)
				}
			}
			w.expect("before the move")

			w.move(w.b)
			w.vendor.lag(queryB, "ENG-1", false)
			w.tick(w.b)
			w.match()
			w.launch()
			onB := intakeOf(w.b, "ENG-1", "1")
			if newFirst {
				w.expect("the new pair judged while the old one waits", "s1 "+onB, "s-b "+onB)
				w.clock.now = w.clock.now.Add(2 * time.Minute)
				w.launch()
			}
			w.expect("once both pairs are judged", "s1 "+onB, "s-b "+onB)
			old := w.pair(onA, "s1")
			why := "its subscription now listens to another connection, trigger or poller"
			if newFirst {
				launched := w.pair(onB, "s1")
				why = "already delivered to this subscription: pair " + launched.ID + " launched run " + launched.RunID + " for the same item revision"
			}
			if old.State != intake.PairRefused || old.Reason != why {
				t.Fatalf("the old pair = %+v, want refused saying %q", old, why)
			}
			if p := w.pair(onB, "s1"); p.State != intake.PairLaunched || !strings.HasPrefix(p.RunID, "run-") {
				t.Fatalf("the new pair = %+v, want launched", p)
			}
			w.newRevision([]string{"s1 " + onB, "s-b " + onB})
		})
	}
}
