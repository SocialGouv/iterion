package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/lease"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// waitForCond polls pred until it holds or the budget runs out: every wait in
// this file is on a condition, the tick durations only pace the sweep.
func waitForCond(t *testing.T, budget time.Duration, what string, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for !pred() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for: %s", budget, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// scanCounter is a replica's lister: it counts the passes that replica ran.
type scanCounter struct{ n atomic.Int64 }

func (c *scanCounter) ListNotifiableRuns(context.Context, time.Time, time.Time, int) ([]mongostore.NotifiableRunRef, error) {
	c.n.Add(1)
	return nil, nil
}

// The fleet shape: several replicas start the sweeper on one shared lease
// store; a replica sweeps only inside a term it was elected to, and when the
// holder stops, another takes over while the stopped one sweeps no more.
// Every replica sweeping is the cost the lease removes: each offer spends
// forge requests, multiplied by the replica count.
//
// The invariant is read from each replica's own "elected" lines, not from a
// count of sweepers: a loaded runner can stall a holder past its renewal
// slack, or step the wall clock the stores compare, and the hand-over that
// follows is the lease working — one more term, logged — not two sweeps. That
// only one replica holds the lease at a time is the store's contract, pinned
// without a clock by the conformance suite in pkg/lease.
func TestGateSweeper_OneReplicaSweepsAndASuccessorTakesOver(t *testing.T) {
	shared := lease.NewMemoryStore()
	type replica struct {
		s     *Server
		log   *lockedBuffer
		scans *scanCounter
		stop  context.CancelFunc
		done  chan struct{}
	}
	const replicas = 3
	rs := make([]*replica, replicas)
	for i := range rs {
		st, err := store.New(t.TempDir())
		if err != nil {
			t.Fatalf("store: %v", err)
		}
		s := newForgeGateTestServer(t, st)
		s.leases = shared
		s.gateSweepTick = 200 * time.Millisecond
		log := &lockedBuffer{}
		s.logger = iterlog.New(iterlog.LevelInfo, log)
		ctx, cancel := context.WithCancel(context.Background())
		r := &replica{s: s, log: log, scans: &scanCounter{}, stop: cancel, done: make(chan struct{})}
		rs[i] = r
		go func() {
			defer close(r.done)
			r.s.runElectedGateSweeper(ctx, r.scans)
		}()
	}
	stopAll := sync.OnceFunc(func() {
		for _, r := range rs {
			r.stop()
			<-r.done
		}
	})
	t.Cleanup(stopAll)

	total := func() (n int64) {
		for _, r := range rs {
			n += r.scans.n.Load()
		}
		return n
	}
	elected := func(r *replica) int {
		return strings.Count(r.log.String(), fmt.Sprintf("lease %q: %s elected", leaseMergeGateSweeper, r.s.replicaID))
	}
	sweptUnelected := func() []int {
		var out []int
		for i, r := range rs {
			if r.scans.n.Load() > 0 && elected(r) == 0 {
				out = append(out, i)
			}
		}
		return out
	}

	waitForCond(t, 10*time.Second, "several sweep passes", func() bool { return total() >= 10 })
	if bad := sweptUnelected(); len(bad) > 0 {
		t.Fatalf("replicas %v swept without ever being elected — every replica sweeping multiplies the forge cost by the replica count", bad)
	}
	leader := rs[0]
	for _, r := range rs[1:] {
		if r.scans.n.Load() > leader.scans.n.Load() {
			leader = r
		}
	}

	leader.stop()
	<-leader.done
	frozen := leader.scans.n.Load()
	var others int64
	for _, r := range rs {
		if r != leader {
			others += r.scans.n.Load()
		}
	}
	waitForCond(t, 10*time.Second, "a successor to sweep", func() bool {
		var n int64
		for _, r := range rs {
			if r != leader {
				n += r.scans.n.Load()
			}
		}
		return n >= others+3
	})
	if got := leader.scans.n.Load(); got != frozen {
		t.Errorf("the stopped leader swept %d more passes", got-frozen)
	}
	if bad := sweptUnelected(); len(bad) > 0 {
		t.Errorf("replicas %v swept without ever being elected", bad)
	}
}

// laneRecordingGateClient records, for every PR read, the lane its request
// would be counted under.
type laneRecordingGateClient struct {
	listingGateClient
	mu    sync.Mutex
	lanes []string
}

func (f *laneRecordingGateClient) GetPullRequest(ctx context.Context, repo string, n int) (forge.PullRef, error) {
	f.mu.Lock()
	f.lanes = append(f.lanes, forgeLaneOf(ctx))
	f.mu.Unlock()
	return f.listingGateClient.GetPullRequest(ctx, repo, n)
}

func (f *laneRecordingGateClient) takeLanes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.lanes
	f.lanes = nil
	return out
}

// The hourly tally is how the sweep's cost is read in production, so the
// sweep's forge requests must be charged to the sweep's lane — and the event
// path's must not be.
func TestGateSweep_ChargesItsForgeRequestsToTheSweepLane(t *testing.T) {
	gc := &laneRecordingGateClient{listingGateClient: listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}}
	s, runID := gateReconcileFixture(t, gatingInputs(), gc)

	s.sweepGates(context.Background(), &fakeGateSweepLister{refs: []mongostore.NotifiableRunRef{{ID: runID}}}, time.Now().UTC(), gateSweepLookback, time.Time{})
	swept := gc.takeLanes()
	if len(swept) == 0 {
		t.Fatal("the sweep never read the pull request")
	}
	for _, lane := range swept {
		if lane != forgeLaneGateSweeper {
			t.Errorf("a sweep request was charged to lane %q, want %q", lane, forgeLaneGateSweeper)
		}
	}

	if err := s.reconcileGateForRun(context.Background(), terminalEvent(runID)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for _, lane := range gc.takeLanes() {
		if lane == forgeLaneGateSweeper {
			t.Error("an event-path request was charged to the sweep lane")
		}
	}
}

// One hour's line, reported once the hour has turned: counted by host, API and
// lane, and marked partial when counting began mid-hour.
func TestForgeRequestTally_ReportsEachHourOnceItTurns(t *testing.T) {
	clock := time.Date(2026, 9, 30, 14, 37, 0, 0, time.UTC)
	var reports []string
	tally := newForgeRequestTally(func() time.Time { return clock }, func(format string, args ...any) {
		reports = append(reports, fmt.Sprintf(format, args...))
	})
	send := func(ctx context.Context, rawURL string) {
		t.Helper()
		u, err := url.Parse(rawURL)
		if err != nil {
			t.Fatal(err)
		}
		tally.observe((&http.Request{Method: http.MethodGet, URL: u}).WithContext(ctx))
	}
	sweep := withForgeLane(context.Background(), forgeLaneGateSweeper)
	send(sweep, "https://api.github.com/repos/o/r/pulls/1")
	send(sweep, "https://api.github.com/repos/o/r/commits/abc/status")
	send(context.Background(), "https://api.github.com/graphql")
	send(context.Background(), "https://api.github.com/repos/o/r/issues")
	if len(reports) != 0 {
		t.Fatalf("reported %q before the hour turned", reports)
	}

	clock = time.Date(2026, 9, 30, 15, 0, 5, 0, time.UTC)
	send(context.Background(), "https://api.github.com/repos/o/r/issues")
	want := "forge HTTP: 4 requests in the hour ending 2026-09-30T15:00Z (counted from 14:37:00Z) — " +
		"api.github.com rest merge-gate-sweeper=2, api.github.com graphql other=1, api.github.com rest other=1"
	if len(reports) != 1 || reports[0] != want {
		t.Fatalf("reports = %q, want exactly %q", reports, want)
	}

	clock = time.Date(2026, 9, 30, 16, 12, 0, 0, time.UTC)
	send(sweep, "https://api.github.com/repos/o/r/pulls/1")
	want = "forge HTTP: 1 requests in the hour ending 2026-09-30T16:00Z — api.github.com rest other=1"
	if len(reports) != 2 || reports[1] != want {
		t.Fatalf("second report = %q, want %q (a whole hour carries no partial note)", reports, want)
	}

	// An idle gap: nothing between 16:12 and 18:05. The next request reports
	// the hour that had requests — dated, so it is not read as the latest.
	clock = time.Date(2026, 9, 30, 18, 5, 0, 0, time.UTC)
	send(context.Background(), "https://api.github.com/repos/o/r/issues")
	want = "forge HTTP: 1 requests in the hour ending 2026-09-30T17:00Z — api.github.com rest merge-gate-sweeper=1"
	if len(reports) != 3 || reports[2] != want {
		t.Fatalf("after a two-hour gap: reports = %q, want a third %q", reports, want)
	}

	// A stop: the hour in progress is reported as far as it went.
	clock = time.Date(2026, 9, 30, 18, 20, 30, 0, time.UTC)
	tally.flush()
	want = "forge HTTP: 1 requests in the hour ending 2026-09-30T19:00Z (until 18:20:30Z, stopping) — api.github.com rest other=1"
	if len(reports) != 4 || reports[3] != want {
		t.Fatalf("flush: reports = %q, want a fourth %q", reports, want)
	}
	tally.flush()
	if len(reports) != 4 {
		t.Fatalf("a second flush reported again: %q", reports[4:])
	}
}

// GraphQL is a separate budget on GitHub, so the split has to be exact: the
// GraphQL endpoint is a path, not a suffix a REST path can also end with.
func TestForgeRequestTally_TellsGraphQLByItsEndpoint(t *testing.T) {
	for path, want := range map[string]string{
		"/graphql":                    "graphql",
		"/api/graphql":                "graphql",
		"/graphql/":                   "graphql",
		"/repos/SocialGouv/graphql":   "rest",
		"/repos/o/r/contents/graphql": "rest",
		"/repos/o/r/pulls/1":          "rest",
	} {
		if got := forgeAPIOf(path); got != want {
			t.Errorf("forgeAPIOf(%q) = %q, want %q", path, got, want)
		}
	}
}

// A forge that controls its redirects controls the host part of the key, so
// one hour names a bounded number of hosts: past the bound, NEW hosts fold
// into one entry, every request is still counted, and a host already named
// keeps its own entries — the sweep's GitHub reads never lose their host
// because other hosts arrived first.
func TestForgeRequestTally_BoundsTheHostsOneHourNames(t *testing.T) {
	clock := time.Date(2026, 9, 30, 14, 0, 0, 0, time.UTC)
	tally := newForgeRequestTally(func() time.Time { return clock }, nil)
	send := func(ctx context.Context, rawURL string) {
		u, _ := url.Parse(rawURL)
		tally.observe((&http.Request{Method: http.MethodGet, URL: u}).WithContext(ctx))
	}
	send(context.Background(), "https://api.github.com/repos/o/r/issues")
	const others = 3 * forgeTallyMaxHosts
	for i := 0; i < others; i++ {
		send(context.Background(), fmt.Sprintf("https://forge-%d.example/api/v4/projects", i))
	}
	sweep := withForgeLane(context.Background(), forgeLaneGateSweeper)
	for i := 0; i < 100; i++ {
		send(sweep, "https://api.github.com/repos/o/r/pulls/1")
	}
	tally.mu.Lock()
	defer tally.mu.Unlock()
	hosts := map[string]bool{}
	var total int64
	for k, n := range tally.counts {
		hosts[k.host] = true
		total += n
	}
	if len(hosts) > forgeTallyMaxHosts+1 {
		t.Errorf("one hour names %d hosts, want at most %d", len(hosts), forgeTallyMaxHosts+1)
	}
	if want := int64(1 + others + 100); total != want {
		t.Errorf("counted %d requests, want %d — folding must not drop any", total, want)
	}
	if tally.counts[forgeTallyKey{host: forgeTallyOtherHosts, api: "rest"}] == 0 {
		t.Error("no request was folded into the other-hosts entry")
	}
	if got := tally.counts[forgeTallyKey{host: "api.github.com", api: "rest", lane: forgeLaneGateSweeper}]; got != 100 {
		t.Errorf("the sweep's reads of a host already named = %d, want 100 under their own host", got)
	}
}

// After an idle gap, the hour a stopping process flushes is the last one it
// counted — whole, not "cut short" at a stop that came hours later.
func TestForgeRequestTally_AFlushAfterAnIdleGapReportsAWholeHour(t *testing.T) {
	clock := time.Date(2026, 9, 30, 15, 50, 0, 0, time.UTC)
	var reports []string
	tally := newForgeRequestTally(func() time.Time { return clock }, func(format string, args ...any) {
		reports = append(reports, fmt.Sprintf(format, args...))
	})
	clock = time.Date(2026, 9, 30, 16, 12, 0, 0, time.UTC)
	u, _ := url.Parse("https://api.github.com/repos/o/r/issues")
	tally.observe((&http.Request{Method: http.MethodGet, URL: u}).WithContext(context.Background()))
	clock = time.Date(2026, 9, 30, 18, 20, 30, 0, time.UTC)
	tally.flush()
	want := "forge HTTP: 1 requests in the hour ending 2026-09-30T17:00Z — api.github.com rest other=1"
	if len(reports) != 1 || reports[0] != want {
		t.Errorf("flush after an idle gap = %q, want %q", reports, want)
	}
}

// The count is only a measurement if it cannot be bypassed: every request the
// forge client sends goes through the tally.
// The shape of the incident is the shape counted here: a redirect hop, then a
// rate-limited 403. Both requests spend budget, so both are counted — a tally
// that counted only successes would read the exhausted half of every hour as
// silence.
func TestForgeHTTPClient_CountsEveryRequestItSends(t *testing.T) {
	var received atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		if r.URL.Path == "/repos/o/r/pulls/1" {
			http.Redirect(w, r, "/repositories/42/pulls/1", http.StatusFound)
			return
		}
		w.Header().Set("X-RateLimit-Remaining", "0")
		http.Error(w, `{"message":"API rate limit exceeded for installation ID 1"}`, http.StatusForbidden)
	}))
	defer srv.Close()
	s := New(Config{}, iterlog.New(iterlog.LevelError, nil))
	client := s.forgeHTTPClient()
	const calls = 3
	for i := 0; i < calls; i++ {
		req, err := http.NewRequestWithContext(withForgeLane(context.Background(), forgeLaneGateSweeper), http.MethodGet, srv.URL+"/repos/o/r/pulls/1", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("request %d ended on %d, want the 403 behind the redirect", i, resp.StatusCode)
		}
	}
	host := srv.Listener.Addr().String()
	s.forgeRequests.mu.Lock()
	got := s.forgeRequests.counts[forgeTallyKey{host: host, api: "rest", lane: forgeLaneGateSweeper}]
	s.forgeRequests.mu.Unlock()
	if got != 2*calls || received.Load() != 2*calls {
		t.Errorf("tally counted %d requests, the forge received %d, want %d each", got, received.Load(), 2*calls)
	}
}

// A stopping process reports the hour it was counting: without it, the
// fleet's hourly sum loses every stopping pod's last hour.
func TestShutdown_ReportsTheForgeHourInProgress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	workDir := t.TempDir()
	out := &lockedBuffer{}
	s := New(Config{
		WorkDir:                 workDir,
		StoreDir:                filepath.Join(workDir, ".iterion"),
		SkipProjectRegistration: true,
	}, iterlog.New(iterlog.LevelInfo, out))
	resp, err := s.forgeHTTPClient().Get(srv.URL + "/repos/o/r/pulls/1")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if logged := out.String(); !strings.Contains(logged, "forge HTTP: 1 requests") || !strings.Contains(logged, "stopping)") {
		t.Errorf("the shutdown did not report the hour in progress; log:\n%s", logged)
	}
}

// dbStore is a run store exposing a database, the shape of the cloud Mongo
// store the lease derivation keys on.
type dbStore struct {
	store.RunStore
	db *mongo.Database
}

func (d dbStore) DB() *mongo.Database { return d.db }

// listerStore is a cloud-shaped run store (it lists runs for the sweep) that
// hides its database — a decorator, say.
type listerStore struct {
	store.RunStore
	*scanCounter
}

// The production form of the election: every replica must campaign on ONE
// shared store. A cloud run store exposing its database gives the Mongo
// lease; a store that lists runs for the sweep but hides its database would
// silently give each replica its own lease — every replica elected — so that
// case must say so.
func TestLeaseStoreFor_EveryReplicaCampaignsOnTheSharedStore(t *testing.T) {
	client, err := mongo.Connect(options.Client().ApplyURI("mongodb://127.0.0.1:1")) // lazy: never dials
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	fs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var warnings []string
	warnf := func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }

	if st := leaseStoreFor(Config{Store: dbStore{RunStore: fs, db: client.Database("iterion")}}, warnf); st == nil {
		t.Fatal("nil lease store")
	} else if _, ok := st.(*lease.MongoStore); !ok {
		t.Errorf("a cloud run store gave %T, want *lease.MongoStore — each replica would elect itself", st)
	}
	injected := lease.NewMemoryStore()
	if st := leaseStoreFor(Config{Store: dbStore{RunStore: fs, db: client.Database("iterion")}, Leases: injected}, warnf); st != injected {
		t.Errorf("Config.Leases was not honoured: got %T", st)
	}
	if st := leaseStoreFor(Config{Store: fs}, warnf); st == nil {
		t.Fatal("nil lease store")
	} else if _, ok := st.(*lease.MemoryStore); !ok {
		t.Errorf("a local store gave %T, want the in-memory store", st)
	}
	if len(warnings) != 0 {
		t.Fatalf("warned without cause: %q", warnings)
	}
	if st := leaseStoreFor(Config{Store: listerStore{RunStore: fs, scanCounter: &scanCounter{}}}, warnf); st == nil {
		t.Fatal("nil lease store")
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "every replica elects itself") {
		t.Errorf("a cloud store hiding its database produced %q, want one warning naming the consequence", warnings)
	}
}

// A holder that loses its lease — a successor took it after this one stalled
// past the TTL — must stop sweeping, or two sweeps run from then on.
func TestGateSweeper_ADeposedHolderStopsSweeping(t *testing.T) {
	shared := lease.NewMemoryStore()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := newForgeGateTestServer(t, st)
	s.leases = shared
	const tick = 100 * time.Millisecond
	s.gateSweepTick = tick
	scans := &scanCounter{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runElectedGateSweeper(ctx, scans)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	waitForCond(t, 10*time.Second, "the replica to sweep", func() bool { return scans.n.Load() >= 3 })

	// The successor's view: this holder's lease expired long ago.
	if ok, err := shared.Acquire(context.Background(), leaseMergeGateSweeper, "successor", time.Now().Add(time.Hour), time.Hour); err != nil || !ok {
		t.Fatalf("successor Acquire = %v, %v", ok, err)
	}
	waitForCond(t, 10*time.Second, "the deposed holder to stop sweeping", func() bool {
		before := scans.n.Load()
		time.Sleep(10 * tick)
		return scans.n.Load() == before
	})
}

// windowRecorder records how far back each pass reached.
type windowRecorder struct {
	mu      sync.Mutex
	windows []time.Duration
}

func (w *windowRecorder) ListNotifiableRuns(_ context.Context, since, before time.Time, _ int) ([]mongostore.NotifiableRunRef, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.windows = append(w.windows, before.Add(gateSweepGrace).Sub(since))
	return nil, nil
}

func (w *windowRecorder) first() (time.Duration, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.windows) == 0 {
		return 0, false
	}
	return w.windows[0], true
}

// The replica that takes the sweep knows nothing of what died while it was not
// sweeping, so its first pass reaches the whole horizon.
func TestGateSweeper_ATermOpensWithADeepPass(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := newForgeGateTestServer(t, st)
	s.gateSweepTick = 20 * time.Millisecond
	rec := &windowRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runElectedGateSweeper(ctx, rec)
	}()
	waitForCond(t, 10*time.Second, "a first pass", func() bool { _, ok := rec.first(); return ok })
	cancel()
	<-done
	if got, _ := rec.first(); got != gateSweepHorizon {
		t.Errorf("the first pass of a term reached back %s, want the whole %s horizon", got, gateSweepHorizon)
	}
}

// laneTaggingStub records the lane of every PR read, for both lanes the sweep
// offers a run to.
type laneTaggingStub struct {
	stubGateClient
	mu    sync.Mutex
	lanes []string
}

func (c *laneTaggingStub) GetPullRequest(ctx context.Context, base string, number int) (forge.PullRef, error) {
	c.mu.Lock()
	c.lanes = append(c.lanes, forgeLaneOf(ctx))
	c.mu.Unlock()
	return c.stubGateClient.GetPullRequest(ctx, base, number)
}

// The sweep offers each run to the reconciler AND to the auto-fix lane; the
// fix lane's reads spend the same installation budget, so they are charged to
// the sweep too.
func TestGateSweep_ChargesTheAutofixOfferToTheSweepLane(t *testing.T) {
	const (
		team  = "t1"
		repo  = "acme/widgets"
		prURL = "https://github.com/acme/widgets/pull/7"
		head  = "cafe1234cafe1234cafe1234cafe1234cafe1234"
	)
	s := newWebhookTestServer(t)
	s.cfg.WorkDir = writeConsumerBotFixture(t, "fixer-bot", "prior_review")
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Store = rs
	conns := forge.NewMemoryConnectionStore()
	if err := conns.Create(context.Background(), forge.Connection{ID: "c1", TenantID: team, Provider: forge.ProviderGitHub}); err != nil {
		t.Fatal(err)
	}
	s.forgeConnections = conns
	s.forgePublishTokens = NewForgePublishTokenRegistry()
	if err := s.forgePublishTokens.Register("run-token", ForgePublishGrant{TeamID: team, ConnectionID: "c1", Repo: repo}); err != nil {
		t.Fatal(err)
	}
	ints := forge.NewMemoryRepoIntegrationStore()
	if err := ints.Create(context.Background(), forge.RepoIntegration{
		ID: "i1", TenantID: team, ConnectionID: "c1", RepoFullName: repo,
		BotIDs: []string{"fixer-bot"}, WebhookID: "w1", AutoFixOnGateFailure: true,
		LaunchVars: map[string]string{gateContextVar: "iterion/review"},
	}); err != nil {
		t.Fatal(err)
	}
	s.forgeIntegrations = ints
	if err := s.webhookConfigs.Create(context.Background(), webhooks.Config{
		ID: "w1", TenantID: team, BotIDs: []string{"fixer-bot"},
	}); err != nil {
		t.Fatal(err)
	}
	gc := &laneTaggingStub{stubGateClient: stubGateClient{head: head, state: forge.CommitStateFailure, ctxName: "iterion/review"}}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
	var launched atomic.Int32
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launched.Add(1)
		return "run-fixer", nil
	}
	id, err := store.GenerateRunID()
	if err != nil {
		t.Fatal(err)
	}
	run, err := rs.CreateRun(context.Background(), id, "reviewer-bot", map[string]any{
		"pr_url": prURL, "gate_context": "iterion/review", "head_sha": head,
		forgePublishVarToken: "run-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	run.BotID = "reviewer-bot"
	run.Status = store.RunStatusFinished
	if err := rs.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}

	s.sweepGates(context.Background(), &fakeGateSweepLister{refs: []mongostore.NotifiableRunRef{{ID: run.ID}}}, time.Now().UTC(), gateSweepLookback, time.Time{})
	if launched.Load() != 1 {
		t.Fatalf("the sweep's autofix offer launched %d fixers, want 1 — the fix lane never ran, so its reads were not observed", launched.Load())
	}
	gc.mu.Lock()
	defer gc.mu.Unlock()
	if len(gc.lanes) < 2 {
		t.Fatalf("observed %d PR reads, want one per lane (reconcile + autofix)", len(gc.lanes))
	}
	for i, lane := range gc.lanes {
		if lane != forgeLaneGateSweeper {
			t.Errorf("PR read %d was charged to lane %q, want %q", i, lane, forgeLaneGateSweeper)
		}
	}
}

// The production line that elects the sweeper — New() deriving the lease
// store from the run store — on the two shapes that matter: a cloud store
// exposing its database campaigns on the shared Mongo lease, and a cloud-shaped
// store hiding it warns through the server's own logger.
func TestNew_WiresTheSharedLeaseStore(t *testing.T) {
	client, err := mongo.Connect(options.Client().ApplyURI("mongodb://127.0.0.1:1")) // lazy: never dials
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	fs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{Store: dbStore{RunStore: fs, db: client.Database("iterion")}}, iterlog.New(iterlog.LevelError, nil))
	if _, ok := s.leases.(*lease.MongoStore); !ok {
		t.Errorf("New() on a cloud run store campaigns on %T, want *lease.MongoStore — every replica would elect itself", s.leases)
	}
	out := &lockedBuffer{}
	New(Config{Store: listerStore{RunStore: fs, scanCounter: &scanCounter{}}}, iterlog.New(iterlog.LevelInfo, out))
	if !strings.Contains(out.String(), "every replica elects itself") {
		t.Errorf("New() on a cloud store hiding its database did not warn; log:\n%s", out.String())
	}
}

// A background loop's last forge request — sent as it winds down — belongs in
// the flushed hour: the flush comes after the join, not before it.
func TestShutdown_TheFlushComesAfterTheJoin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	workDir := t.TempDir()
	out := &lockedBuffer{}
	s := New(Config{
		WorkDir:                 workDir,
		StoreDir:                filepath.Join(workDir, ".iterion"),
		SkipProjectRegistration: true,
	}, iterlog.New(iterlog.LevelInfo, out))
	s.goUntilShutdown("test.lastRequest", func(ctx context.Context) {
		<-ctx.Done()
		if resp, err := s.forgeHTTPClient().Get(srv.URL + "/repos/o/r/pulls/1"); err == nil {
			_ = resp.Body.Close()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if logged := out.String(); !strings.Contains(logged, "forge HTTP: 1 requests") {
		t.Errorf("the last request of a winding-down loop is in no hourly line; log:\n%s", logged)
	}
}

// Every term opens deep, not only a process's first: a replica deposed and
// later re-elected has missed what died meanwhile, like one that just started.
func TestGateSweeper_EveryTermOpensWithADeepPass(t *testing.T) {
	shared := lease.NewMemoryStore()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := newForgeGateTestServer(t, st)
	s.leases = shared
	const tick = 100 * time.Millisecond
	s.gateSweepTick = tick
	rec := &windowRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runElectedGateSweeper(ctx, rec)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	passes := func() int {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.windows)
	}
	waitForCond(t, 10*time.Second, "a few passes", func() bool { return passes() >= 3 })
	if ok, err := shared.Acquire(context.Background(), leaseMergeGateSweeper, "successor", time.Now().Add(time.Hour), time.Hour); err != nil || !ok {
		t.Fatalf("successor Acquire = %v, %v", ok, err)
	}
	waitForCond(t, 10*time.Second, "the deposed holder to stop", func() bool {
		before := passes()
		time.Sleep(10 * tick)
		return passes() == before
	})
	first := passes()
	if err := shared.Release(context.Background(), leaseMergeGateSweeper, "successor"); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, 10*time.Second, "the second term's first pass", func() bool { return passes() > first })
	rec.mu.Lock()
	got := rec.windows[first]
	rec.mu.Unlock()
	if got != gateSweepHorizon {
		t.Errorf("the second term's first pass reached back %s, want the whole %s horizon", got, gateSweepHorizon)
	}
}
