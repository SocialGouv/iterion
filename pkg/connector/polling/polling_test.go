package polling

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/exec"
	"github.com/SocialGouv/iterion/pkg/connector/intake"
	"github.com/SocialGouv/iterion/pkg/connector/poller"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/lease"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// vendor is a search API answering what a JQL-style poll asks: the issues
// updated at or after `since` (epoch milliseconds, unquoted), oldest first,
// pageSize to a page behind an opaque `next` token. It is written from the
// vendor's side — it reads its own query string — never from the package.
type vendor struct {
	mu       sync.Mutex
	issues   []map[string]any
	pageSize int
	requests []string
	srv      *httptest.Server
}

func newVendor(t *testing.T) *vendor {
	v := &vendor{pageSize: 50}
	v.srv = httptest.NewServer(http.HandlerFunc(v.serve))
	t.Cleanup(v.srv.Close)
	return v
}

func (v *vendor) serve(w http.ResponseWriter, r *http.Request) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.requests = append(v.requests, r.URL.RawQuery)
	if r.URL.Path != "/search" || r.Header.Get("Authorization") != "Bearer vendor-token" {
		http.Error(w, "no such resource", http.StatusNotFound)
		return
	}
	var since time.Time
	if s := r.URL.Query().Get("since"); s != "" {
		ms, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			http.Error(w, "since is not epoch milliseconds", http.StatusBadRequest)
			return
		}
		since = time.UnixMilli(ms)
	}
	match := []map[string]any{}
	for _, is := range v.issues {
		at, _ := time.Parse(time.RFC3339Nano, is["updated"].(string))
		if !at.Before(since) {
			match = append(match, is)
		}
	}
	slices.SortStableFunc(match, func(a, b map[string]any) int { return strings.Compare(a["updated"].(string), b["updated"].(string)) })
	start, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
	end := min(start+v.pageSize, len(match))
	page := map[string]any{"issues": match[start:end]}
	if end < len(match) {
		page["next"] = strconv.Itoa(end)
	}
	_ = json.NewEncoder(w).Encode(page)
}

func (v *vendor) add(key, title string, updated time.Time, extra map[string]any) {
	v.mu.Lock()
	defer v.mu.Unlock()
	is := map[string]any{"key": key, "title": title, "updated": updated.UTC().Format(time.RFC3339Nano)}
	for k, x := range extra {
		is[k] = x
	}
	for i, cur := range v.issues {
		if cur["key"] == key {
			v.issues[i] = is
			return
		}
	}
	v.issues = append(v.issues, is)
}

func (v *vendor) calls() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.requests...)
}

func vendorPackage() *spec.Package {
	return &spec.Package{
		Connector: spec.Connector{
			SchemaVersion: spec.SchemaVersion, ID: "probe", Version: "1.0.0",
			BaseURL:         spec.BaseURL{OperatorSupplied: true},
			Auth:            []spec.AuthScheme{{ID: "token", Kind: spec.AuthBearer}},
			DefaultSecurity: []spec.SecurityRequirement{{Terms: []spec.SecurityTerm{{SchemeID: "token"}}}},
			Maturity:        spec.MaturityExperimental,
		},
		Ops: []spec.OpsFile{{SchemaVersion: spec.SchemaVersion, Connector: "probe", Domain: "issue", Operations: []spec.Operation{{
			ID: "probe.issue.search", Resource: "issue", Verb: "search",
			HTTP: spec.HTTPBinding{Method: "GET", Path: "/search"},
			Params: []spec.Param{
				{Key: "q", Name: "q", In: spec.InQuery, Type: "string", Required: true},
				{Key: "since", Name: "since", In: spec.InQuery, Type: "string"},
				{Key: "cursor", Name: "cursor", In: spec.InQuery, Type: "string"},
			},
			Pagination: &spec.Pagination{Style: spec.PageCursor, CursorParam: "cursor", CursorField: "next", ItemsField: "issues", MaxPages: 3},
			Results:    []spec.ResultCase{{Status: 200}},
			Effect:     spec.EffectRead, Deterministic: true,
		}}}},
		Triggers: []spec.Trigger{{
			ID: "probe.issue.changed", Delivery: spec.DeliveryPoll,
			Poll: &spec.TriggerPoll{
				Operation: "probe.issue.search",
				Params:    map[string]string{"q": "query.q", "since": "cursor.epoch_ms"},
				Query:     []spec.PollQueryField{{ID: "q", Required: true}},
				ItemID:    "item.key", ItemVersion: "item.updated",
				Cursor:   &spec.PollCursor{From: "item.updated", Kind: spec.CursorTimestamp, Overlap: "1m"},
				Interval: spec.PollInterval{Default: "5m", Min: "1m"},
			},
			Ignore:  "payload.bot == true",
			Outputs: []spec.TriggerOutput{{Name: "subject", From: "payload.title"}},
		}},
	}
}

var t0 = time.Date(2026, 11, 18, 23, 0, 0, 0, time.UTC)

// world is one tenant's connection, subscription and stores, around one
// vendor.
type world struct {
	t       *testing.T
	vendor  *vendor
	pollers *poller.MemoryStore
	intakes *intake.MemoryStore
	subs    *trigger.MemorySubscriptionStore
	conns   *connection.MemoryStore
	sealer  secrets.Sealer
	catalog connection.Catalog
	poller  poller.Poller
	clock   time.Time
	mu      sync.Mutex
}

func newWorld(t *testing.T) *world {
	t.Helper()
	sealer, err := secrets.NewAESGCMSealer([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	w := &world{
		t: t, vendor: newVendor(t), pollers: poller.NewMemoryStore(), intakes: intake.NewMemoryStore(),
		subs: trigger.NewMemorySubscriptionStore(), conns: connection.NewMemoryStore(), sealer: sealer,
		catalog: connection.NewMemoryCatalog(vendorPackage()), clock: t0,
	}
	sealed, err := connection.SealToken(sealer, "conn-1", "vendor-token", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.conns.Create(context.Background(), connection.Connection{
		ID: "conn-1", TenantID: "t1", Connector: "probe", Alias: "main", SchemeID: "token",
		Status: connection.StatusActive, Capabilities: []connection.Capability{connection.CapAction, connection.CapTrigger},
		BaseURL: w.vendor.srv.URL, AuthPlacement: connection.AuthPlacement{Kind: "bearer"}, SealedPayload: sealed,
	}); err != nil {
		t.Fatal(err)
	}
	query := map[string]string{"q": "project = ENG"}
	p, _, err := w.pollers.Ensure(context.Background(), poller.Poller{TenantID: "t1", ConnectionID: "conn-1", TriggerID: "probe.issue.changed", Query: query, CreatedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	w.poller = p
	return w
}

func (w *world) subscribe(id string, since int64, backfill int) {
	w.t.Helper()
	if err := w.subs.Create(context.Background(), trigger.Subscription{
		ID: id, TenantID: "t1", BotID: "bot-" + id, Enabled: true, Invocation: bundle.InvocationKindConnector,
		ConnectionID: "conn-1", TriggerID: "probe.issue.changed", Poller: w.poller.ID,
		PollQuery: map[string]string{"q": "project = ENG"}, PollerSince: since, Backfill: backfill,
	}); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) now() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.clock
}

func (w *world) advance(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.clock = w.clock.Add(d)
}

func (w *world) worker(owner string, client *http.Client) *Worker {
	if client == nil {
		// The test server is loopback, which the guarded dialer refuses on
		// purpose; MarkGuarded is where the test declares that one exemption
		// (TestAFeedOnAPrivateAddressIsRefused drives the real guard).
		client = exec.MarkGuarded(w.vendor.srv.Client())
	}
	return &Worker{
		Pollers: w.pollers, Intakes: w.intakes, Subscriptions: w.subs,
		Catalog: func(string) connection.Catalog { return w.catalog },
		Resolver: func(tenant string) *connection.Resolver {
			return &connection.Resolver{Catalog: w.catalog, Store: w.conns, Sealer: w.sealer, TenantID: tenant}
		},
		Client: client, Owner: owner, Policy: Policy{SeenRetention: DefaultSeenRetention, IntakeRetention: 30 * 24 * time.Hour},
		Now: w.now,
	}
}

func (w *world) stored() poller.Poller {
	w.t.Helper()
	p, err := w.pollers.Get(context.Background(), "t1", w.poller.ID)
	if err != nil {
		w.t.Fatal(err)
	}
	return p
}

func (w *world) pass(wk *Worker) {
	w.t.Helper()
	if _, err := wk.Pass(context.Background()); err != nil {
		w.t.Fatalf("pass: %v", err)
	}
}

// intakesBy lists the poller's intakes by sequence, lowest first.
func (w *world) intakesBy() []intake.Intake {
	w.t.Helper()
	rows, err := w.intakes.ListPollerIntakes(context.Background(), "t1", w.poller.ID, 1<<62, 0)
	if err != nil {
		w.t.Fatal(err)
	}
	slices.Reverse(rows)
	return rows
}

func keysOf(rows []intake.Intake) []string {
	out := make([]string, 0, len(rows))
	for _, in := range rows {
		var ev struct {
			Payload map[string]any `json:"payload"`
		}
		_ = json.Unmarshal(in.Event, &ev)
		out = append(out, ev.Payload["key"].(string)+"@"+in.State)
	}
	return out
}

// The first scan of a new poller records what is already there as primed —
// durable, matched to nothing — so enabling a source never replays its
// backlog; an item that appears after it is pending, numbered after them.
func TestTheFirstTickFiresNothingAndALaterItemDoes(t *testing.T) {
	w := newWorld(t)
	w.subscribe("s1", 0, 0)
	w.vendor.add("ENG-1", "Intake stalls", t0.Add(-time.Hour), nil)
	w.vendor.add("ENG-2", "Retry storm", t0.Add(-30*time.Minute), nil)
	wk := w.worker("w1", nil)

	w.pass(wk)
	if got := keysOf(w.intakesBy()); !slices.Equal(got, []string{"ENG-1@primed", "ENG-2@primed"}) {
		t.Fatalf("after the first tick = %v, want both primed", got)
	}
	p := w.stored()
	if !p.Primed || p.PrimedThrough != 2 || p.Checkpoint == nil || !p.Checkpoint.At.Equal(t0.Add(-30*time.Minute)) {
		t.Fatalf("poller = primed %v through %d checkpoint %+v, want primed through 2 at ENG-2's instant", p.Primed, p.PrimedThrough, p.Checkpoint)
	}
	if claimed, _ := w.intakes.ClaimIntakes(context.Background(), "m", w.now().Add(time.Hour), time.Minute, 10); len(claimed) != 0 {
		t.Fatalf("the matcher can claim %d primed intakes", len(claimed))
	}

	w.vendor.add("ENG-3", "New one", t0.Add(time.Minute), nil)
	w.advance(5 * time.Minute)
	w.pass(wk)
	got := w.intakesBy()
	if keys := keysOf(got); !slices.Equal(keys, []string{"ENG-1@primed", "ENG-2@primed", "ENG-3@pending"}) {
		t.Fatalf("after the second tick = %v, want ENG-3 pending and nothing re-accepted", keys)
	}
	if got[2].Sequence != 3 {
		t.Fatalf("ENG-3 sequence = %d, want 3", got[2].Sequence)
	}
	// The second scan asked from the checkpoint moved back by the overlap,
	// as an unquoted epoch-milliseconds number.
	calls := w.vendor.calls()
	want := "since=" + strconv.FormatInt(t0.Add(-31*time.Minute).UnixMilli(), 10)
	if last := calls[len(calls)-1]; !strings.Contains(last, want) {
		t.Fatalf("second scan asked %q, want %s", last, want)
	}
}

// Two replicas ticking one poller: the generation lets one through, and the
// other reads nothing — in both orders.
func TestTwoReplicasTickOnePollerOnce(t *testing.T) {
	for _, order := range [][2]string{{"r1", "r2"}, {"r2", "r1"}} {
		t.Run(order[0]+" first", func(t *testing.T) {
			w := newWorld(t)
			w.subscribe("s1", 0, 0)
			w.vendor.add("ENG-1", "a", t0.Add(-time.Hour), nil)
			snapshot := w.poller // both replicas listed it due at generation 0
			a, b := w.worker(order[0], nil), w.worker(order[1], nil)
			if err := a.Tick(context.Background(), snapshot); err != nil {
				t.Fatal(err)
			}
			if err := b.Tick(context.Background(), snapshot); err != nil {
				t.Fatal(err)
			}
			if n := len(w.vendor.calls()); n != 1 {
				t.Fatalf("the vendor was asked %d times, want once", n)
			}
			if p := w.stored(); p.Generation != 1 || p.Holder != order[0] || len(w.intakesBy()) != 1 {
				t.Fatalf("poller generation %d holder %q intakes %d, want the first replica's single tick", p.Generation, p.Holder, len(w.intakesBy()))
			}
		})
	}
	// Concurrently: exactly one tick reaches the vendor.
	w := newWorld(t)
	w.subscribe("s1", 0, 0)
	w.vendor.add("ENG-1", "a", t0.Add(-time.Hour), nil)
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			if err := w.worker(owner, nil).Tick(context.Background(), w.poller); err != nil {
				t.Errorf("%s: %v", owner, err)
			}
		}("r" + strconv.Itoa(i))
	}
	wg.Wait()
	if n := len(w.vendor.calls()); n != 1 {
		t.Fatalf("concurrent replicas asked the vendor %d times, want once", n)
	}
}

// A feed on a private address is refused by the guarded client the server
// hands the worker: nothing is accepted, nothing primed, and the record says
// why.
func TestAFeedOnAPrivateAddressIsRefused(t *testing.T) {
	w := newWorld(t)
	w.subscribe("s1", 0, 0)
	w.vendor.add("ENG-1", "a", t0.Add(-time.Hour), nil)
	w.pass(w.worker("w1", connection.CloudHTTPClient()))
	if n := len(w.vendor.calls()); n != 0 {
		t.Fatalf("the vendor on a loopback address was reached %d times", n)
	}
	p := w.stored()
	if p.Primed || p.Checkpoint != nil || len(w.intakesBy()) != 0 || p.LastError == "" {
		t.Fatalf("poller = primed %v checkpoint %+v intakes %d error %q, want nothing moved and the refusal recorded",
			p.Primed, p.Checkpoint, len(w.intakesBy()), p.LastError)
	}
	if !strings.Contains(p.LastError, "not a public") && !strings.Contains(p.LastError, "private") && !strings.Contains(p.LastError, "loopback") {
		t.Fatalf("last error = %q, want it to name the refused address", p.LastError)
	}
}

// A scan that stops before its last page keeps what it accepted but moves
// neither the checkpoint nor the priming: the next scan re-reads from the
// old place, and the seen set absorbs what it re-reads.
func TestAnIncompleteScanNeverMovesTheCheckpoint(t *testing.T) {
	w := newWorld(t)
	w.subscribe("s1", 0, 0)
	w.vendor.pageSize = 1
	for i := range 4 {
		w.vendor.add("ENG-"+strconv.Itoa(i+1), "x", t0.Add(time.Duration(i-10)*time.Minute), nil)
	}
	wk := w.worker("w1", nil)
	w.pass(wk)
	p := w.stored()
	if p.Primed || p.Checkpoint != nil || !strings.Contains(p.LastError, "before the last page") {
		t.Fatalf("after an incomplete scan = primed %v checkpoint %+v error %q, want nothing moved and the reason", p.Primed, p.Checkpoint, p.LastError)
	}
	if got := keysOf(w.intakesBy()); !slices.Equal(got, []string{"ENG-1@primed", "ENG-2@primed", "ENG-3@primed"}) {
		t.Fatalf("intakes = %v, want the three pages it read, primed", got)
	}
	w.vendor.pageSize = 50
	w.advance(5 * time.Minute)
	w.pass(wk)
	p = w.stored()
	if !p.Primed || p.LastError != "" || p.Checkpoint == nil || !p.Checkpoint.At.Equal(t0.Add(-7*time.Minute)) {
		t.Fatalf("after a complete scan = primed %v checkpoint %+v error %q", p.Primed, p.Checkpoint, p.LastError)
	}
	if got := keysOf(w.intakesBy()); !slices.Equal(got, []string{"ENG-1@primed", "ENG-2@primed", "ENG-3@primed", "ENG-4@primed"}) {
		t.Fatalf("intakes = %v, want ENG-4 added once and nothing doubled", got)
	}
}

// What the trigger's loop guard drops is observed, not accepted; a new
// revision of an accepted item is a new intake.
func TestTheLoopGuardDropsAndANewRevisionIsANewIntake(t *testing.T) {
	w := newWorld(t)
	w.subscribe("s1", 0, 0)
	wk := w.worker("w1", nil)
	w.pass(wk) // primes an empty poller
	w.vendor.add("ENG-1", "by a bot", t0.Add(time.Minute), map[string]any{"bot": true})
	w.vendor.add("ENG-2", "by a person", t0.Add(2*time.Minute), nil)
	w.advance(5 * time.Minute)
	w.pass(wk)
	w.vendor.add("ENG-2", "edited", t0.Add(6*time.Minute), nil)
	w.advance(5 * time.Minute)
	w.pass(wk)
	if got := keysOf(w.intakesBy()); !slices.Equal(got, []string{"ENG-2@pending", "ENG-2@pending"}) {
		t.Fatalf("intakes = %v, want ENG-2 twice (two revisions) and the bot's change dropped", got)
	}
	if seen, _ := w.pollers.Seen(context.Background(), w.poller.ID, []poller.ItemKey{{ID: "ENG-1", Version: t0.Add(time.Minute).Format(time.RFC3339Nano)}}); len(seen) != 1 {
		t.Fatalf("the dropped item is not in the seen set: it would be judged again on every tick")
	}
}

// Nobody listening: the vendor is not called, and the poller waits.
func TestAPollerNobodyListensToReadsNothing(t *testing.T) {
	w := newWorld(t)
	w.vendor.add("ENG-1", "a", t0.Add(-time.Hour), nil)
	w.pass(w.worker("w1", nil))
	if n := len(w.vendor.calls()); n != 0 {
		t.Fatalf("an unsubscribed poller called the vendor %d times", n)
	}
	if p := w.stored(); p.Primed || !p.NextTickAt.Equal(t0.Add(dormantRecheck)) {
		t.Fatalf("poller = primed %v next %s, want it waiting unprimed", p.Primed, p.NextTickAt)
	}
}

// A subscription's backfill pairs the last N intakes its activation and the
// priming reached, for it alone, once.
func TestABackfillPairsTheLastPrimedIntakesForItsSubscriptionAlone(t *testing.T) {
	w := newWorld(t)
	w.subscribe("s-back", 0, 2)
	w.subscribe("s-plain", 0, 0)
	for i := range 3 {
		w.vendor.add("ENG-"+strconv.Itoa(i+1), "x", t0.Add(time.Duration(i-10)*time.Minute), nil)
	}
	wk := w.worker("w1", nil)
	w.pass(wk)
	pairs, err := w.intakes.ListPairs(context.Background(), "t1", intake.PairQuery{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	rows := w.intakesBy()
	var got []string
	for _, p := range pairs {
		if p.SubscriptionID != "s-back" || p.State != intake.PairPending {
			t.Fatalf("pair %+v, want s-back's pending pairs only", p)
		}
		for _, in := range rows {
			if in.ID == p.IntakeID {
				got = append(got, strconv.FormatInt(in.Sequence, 10))
			}
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"2", "3"}) {
		t.Fatalf("backfilled sequences = %v, want the last two primed (2, 3)", got)
	}
	sub, _ := w.subs.Get(context.Background(), "s-back")
	if !sub.BackfillDone {
		t.Fatal("the backfill is not recorded done: it would be paired again")
	}
	w.advance(5 * time.Minute)
	w.pass(wk)
	if again, _ := w.intakes.ListPairs(context.Background(), "t1", intake.PairQuery{}, 0); len(again) != 2 {
		t.Fatalf("pairs after another tick = %d, want the backfill once", len(again))
	}
}

// subs lists the stored subscription.
func (w *world) sub(id string) trigger.Subscription {
	w.t.Helper()
	s, err := w.subs.Get(context.Background(), id)
	if err != nil {
		w.t.Fatal(err)
	}
	return s
}

// setEnabled pauses or resumes a subscription the way the save does: a
// resume is an activation at the poller's current sequence, woken at once.
func (w *world) setEnabled(id string, on bool) {
	w.t.Helper()
	s := w.sub(id)
	s.Enabled = on
	if on {
		p := w.stored()
		s.PollerSince, s.BackfillDone = p.Sequence, false
		if err := w.pollers.Wake(context.Background(), "t1", p.ID, w.now()); err != nil {
			w.t.Fatal(err)
		}
	}
	if err := w.subs.Update(context.Background(), s); err != nil {
		w.t.Fatal(err)
	}
}

// pending lists the keys of the poller's intakes the matcher may launch.
func (w *world) pending() []string {
	var out []string
	for _, k := range keysOf(w.intakesBy()) {
		if strings.HasSuffix(k, "@pending") {
			out = append(out, k)
		}
	}
	return out
}

// What changed while a poller's only subscription was paused is never
// launched when it resumes: the poller went dormant, and its first scan
// after the resume primes what it finds.
func TestAResumeAfterAPauseLaunchesNothingFromThePause(t *testing.T) {
	w := newWorld(t)
	w.subscribe("s1", 0, 0)
	w.vendor.add("ENG-1", "before", t0.Add(-time.Hour), nil)
	wk := w.worker("w1", nil)
	w.pass(wk)

	w.setEnabled("s1", false)
	w.advance(5 * time.Minute)
	w.pass(wk) // nobody listens: dormant
	if p := w.stored(); p.Primed || !p.NextTickAt.Equal(w.now().Add(dormantRecheck)) {
		t.Fatalf("an unlistened poller = primed %v next %s, want it unprimed and dormant", p.Primed, p.NextTickAt)
	}
	for i := range 5 {
		w.vendor.add("ENG-1"+strconv.Itoa(i), "during the pause", w.now().Add(time.Duration(i+1)*time.Minute), nil)
	}
	w.advance(10 * time.Minute)
	w.setEnabled("s1", true)
	w.pass(wk)
	if got := w.pending(); len(got) != 0 {
		t.Fatalf("the resume launched %v, want nothing that changed during the pause", got)
	}
	if p := w.stored(); !p.Primed {
		t.Fatal("the poller did not prime again after its dormancy")
	}
	w.vendor.add("ENG-99", "after", w.now().Add(time.Minute), nil)
	w.advance(5 * time.Minute)
	w.pass(wk)
	if got := w.pending(); !slices.Equal(got, []string{"ENG-99@pending"}) {
		t.Fatalf("after the resume = %v, want the new change alone", got)
	}
}

// A new subscription on a poller nobody listened to for months receives
// nothing of what changed meanwhile — and the dormant scan read only what
// changed since nobody listened, never the months before.
func TestANewSubscriptionOnADormantPollerLaunchesNoBacklog(t *testing.T) {
	w := newWorld(t)
	w.subscribe("s1", 0, 0)
	wk := w.worker("w1", nil)
	w.pass(wk)
	if err := w.subs.Delete(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	w.advance(5 * time.Minute)
	w.pass(wk)
	for i := range 7 {
		w.vendor.add("OLD-"+strconv.Itoa(i), "old", w.now().Add(time.Duration(i+1)*time.Hour), nil)
	}
	w.advance(84 * 24 * time.Hour)
	w.subscribe("s3", w.stored().Sequence, 0)
	if err := w.pollers.Wake(context.Background(), "t1", w.poller.ID, w.now()); err != nil {
		t.Fatal(err)
	}
	w.pass(wk)
	if got := w.pending(); len(got) != 0 {
		t.Fatalf("a new subscription on a dormant poller launched %v, want no backlog", got)
	}
}

// One item a vendor stamped in the future does not move the checkpoint
// past the tick's clock: the changes after it are still read.
func TestAFutureInstantDoesNotBlindThePoller(t *testing.T) {
	w := newWorld(t)
	w.subscribe("s1", 0, 0)
	wk := w.worker("w1", nil)
	w.pass(wk)
	w.vendor.add("ENG-1", "imported", w.now().Add(30*24*time.Hour), nil)
	w.advance(5 * time.Minute)
	w.pass(wk)
	if p := w.stored(); p.Checkpoint == nil || p.Checkpoint.At.After(w.now()) {
		t.Fatalf("checkpoint = %+v, want it no later than the tick's clock %s", p.Checkpoint, w.now())
	}
	w.vendor.add("ENG-2", "real", w.now().Add(time.Minute), nil)
	w.advance(5 * time.Minute)
	w.pass(wk)
	if got := w.pending(); !slices.Contains(got, "ENG-2@pending") {
		t.Fatalf("pending = %v, want the real change read after the future one", got)
	}
}

// An item matched between a save's read of the poller's sequence and the
// subscription landing is owed to it all the same: the first settled tick
// pairs it, once — and never a primed intake.
func TestAnActivationIsCaughtUpOnWhatWasMatchedWithoutIt(t *testing.T) {
	w := newWorld(t)
	w.subscribe("s1", 0, 0)
	wk := w.worker("w1", nil)
	w.pass(wk)
	w.vendor.add("ENG-9", "in the window", t0.Add(time.Minute), nil)
	since := w.stored().Sequence // the save of s2 reads the sequence here
	w.advance(5 * time.Minute)
	w.pass(wk) // ENG-9 accepted, matched against s1 only
	w.subscribe("s2", since, 0)
	w.advance(5 * time.Minute)
	w.pass(wk)
	pairs, err := w.intakes.ListPairs(context.Background(), "t1", intake.PairQuery{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var s2 []string
	for _, p := range pairs {
		if p.SubscriptionID == "s2" {
			s2 = append(s2, p.State)
		}
	}
	if !slices.Equal(s2, []string{intake.PairPending}) || !w.sub("s2").BackfillDone {
		t.Fatalf("s2 pairs = %v settled %v, want ENG-9 paired once and the activation settled", s2, w.sub("s2").BackfillDone)
	}
	w.advance(5 * time.Minute)
	w.pass(wk)
	if again, _ := w.intakes.ListPairs(context.Background(), "t1", intake.PairQuery{}, 0); len(again) != len(pairs) {
		t.Fatalf("pairs after another tick = %d, want %d: a settled activation is not caught up again", len(again), len(pairs))
	}
}

// A slow vendor costs its own poller's tick, bounded, not every tenant's
// pass: the other poller of the same pass is read.
func TestASlowVendorDoesNotHoldTheOthersPollers(t *testing.T) {
	w := newWorld(t)
	w.subscribe("s1", 0, 0)
	slow := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer slow.Close()
	other := connection.Connection{
		ID: "conn-2", TenantID: "t2", Connector: "probe", Alias: "main", SchemeID: "token",
		Status: connection.StatusActive, Capabilities: []connection.Capability{connection.CapAction, connection.CapTrigger},
		BaseURL: slow.URL, AuthPlacement: connection.AuthPlacement{Kind: "bearer"},
	}
	var err error
	if other.SealedPayload, err = connection.SealToken(w.sealer, "conn-2", "vendor-token", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := w.conns.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	q := map[string]string{"q": "project = OPS"}
	sp, _, err := w.pollers.Ensure(context.Background(), poller.Poller{TenantID: "t2", ConnectionID: "conn-2", TriggerID: "probe.issue.changed", Query: q, CreatedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.subs.Create(context.Background(), trigger.Subscription{
		ID: "s-slow", TenantID: "t2", BotID: "bot-slow", Enabled: true, Invocation: bundle.InvocationKindConnector,
		ConnectionID: "conn-2", TriggerID: "probe.issue.changed", Poller: sp.ID, PollQuery: q,
	}); err != nil {
		t.Fatal(err)
	}
	wk := w.worker("w1", nil)
	wk.Client = exec.MarkGuarded(&http.Client{})
	wk.Policy.TickTimeout, wk.Policy.Concurrency = 300*time.Millisecond, 2
	start := time.Now()
	w.pass(wk)
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the pass took %s behind a slow vendor, want it bounded by the tick deadline", took)
	}
	if p := w.stored(); !p.Primed {
		t.Fatal("the fast tenant's poller was not read in the pass the slow one held")
	}
	slowP, _ := w.pollers.Get(context.Background(), "t2", sp.ID)
	if slowP.Primed || slowP.LastError == "" {
		t.Fatalf("the slow poller = primed %v error %q, want its timeout recorded", slowP.Primed, slowP.LastError)
	}
}

// A pass whose every call fails to reach its vendor says so, for the lease
// to hand the term over; one that read a vendor progressed; one that called
// nobody is idle.
func TestAPassSaysWhetherItReachedItsVendors(t *testing.T) {
	w := newWorld(t)
	wk := w.worker("w1", nil)
	if v, _ := wk.Pass(context.Background()); v != lease.VerdictIdle {
		t.Fatalf("a pass with nobody listening = %v, want idle", v)
	}
	w.subscribe("s1", 0, 0)
	w.advance(dormantRecheck)
	if v, _ := wk.Pass(context.Background()); v != lease.VerdictProgressed {
		t.Fatalf("a pass that read its vendor = %v, want progressed", v)
	}
	w.vendor.srv.Close()
	w.advance(5 * time.Minute)
	if v, _ := wk.Pass(context.Background()); v != lease.VerdictUnreachable {
		t.Fatalf("a pass whose vendor is gone = %v, want unreachable", v)
	}
}

// A poller that stops reading says so where an operator reads it — its
// subscriptions' last error — and clears it once it reads again.
func TestAFailingPollerSaysSoOnItsSubscriptions(t *testing.T) {
	w := newWorld(t)
	w.subscribe("s1", 0, 0)
	wk := w.worker("w1", nil)
	w.pass(wk)
	conn, _ := w.conns.Get(context.Background(), "t1", "conn-1")
	conn.Capabilities = []connection.Capability{connection.CapTrigger}
	if err := w.conns.Update(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	w.advance(5 * time.Minute)
	w.pass(wk)
	if got := w.sub("s1").LastError; !strings.HasPrefix(got, "poller: ") || !strings.Contains(got, "action") {
		t.Fatalf("subscription last error = %q, want the poller's refusal naming the missing grant", got)
	}
	conn, _ = w.conns.Get(context.Background(), "t1", "conn-1")
	conn.Capabilities = []connection.Capability{connection.CapTrigger, connection.CapAction}
	if err := w.conns.Update(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	w.advance(5 * time.Minute)
	w.pass(wk)
	if got := w.sub("s1").LastError; got != "" {
		t.Fatalf("subscription last error = %q after the poller read again, want it cleared", got)
	}
}
