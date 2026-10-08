package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// budgetGitHub is a GitHub Enterprise host serving two App installations. It
// mints installation tokens and lists an installation's repositories, and —
// as GitHub does — reports a budget on every answer, 2xx included: each
// installation's own on the calls its token authenticates, the App's on the
// mint its JWT authenticates.
type budgetGitHub struct {
	srv *httptest.Server

	mu        sync.Mutex
	remaining map[string]int64 // by installation id, decremented per call
	limit     map[string]int64
	reset     map[string]time.Time
}

// budgetGitHubAppRemaining is the App budget the mint reports: lower than any
// installation's, so a gauge that folded the App's JWT calls into an
// installation would show it.
const budgetGitHubAppRemaining = 3

func newBudgetGitHub(t *testing.T) *budgetGitHub {
	t.Helper()
	f := &budgetGitHub{remaining: map[string]int64{}, limit: map[string]int64{}, reset: map[string]time.Time{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *budgetGitHub) installation(id string, remaining, limit int64, reset time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.remaining[id], f.limit[id], f.reset[id] = remaining, limit, reset
}

func (f *budgetGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	const mintPrefix = "/api/v3/app/installations/"
	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, mintPrefix) && strings.HasSuffix(r.URL.Path, "/access_tokens"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, mintPrefix), "/access_tokens")
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(budgetGitHubAppRemaining))
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(f.reset[id].Unix(), 10))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_inst" + id,
			"expires_at": time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
		})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v3/installation/repositories":
		id := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ghs_inst")
		if _, ok := f.remaining[id]; !ok {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		f.remaining[id]--
		w.Header().Set("X-RateLimit-Remaining", strconv.FormatInt(f.remaining[id], 10))
		w.Header().Set("X-RateLimit-Limit", strconv.FormatInt(f.limit[id], 10))
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(f.reset[id].Unix(), 10))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"repositories":[]}`))
	default:
		http.NotFound(w, r)
	}
}

// tallyClock is the tally's clock in a test: the instant each forge answer is
// seen is the one set before the call that receives it.
type tallyClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *tallyClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func (c *tallyClock) read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// The production path of #2105: two GitHub App installations on one host,
// each read through its connection's App client. The hour's report names
// each installation's lowest remaining budget, when it was seen and when its
// window resets — the installation's own, never the App's that its token
// mint reported.
func TestForgeRequestTally_ReportsEachInstallationsLowestBudgetOfTheHour(t *testing.T) {
	f := newBudgetGitHub(t)
	reset101 := time.Date(2026, 9, 30, 14, 45, 0, 0, time.UTC)
	reset202 := time.Date(2026, 9, 30, 14, 58, 0, 0, time.UTC)
	f.installation("101", 4991, 5000, reset101)
	f.installation("202", 11951, 12500, reset202)

	s := New(Config{}, iterlog.New(iterlog.LevelError, nil))
	s.forgeGitHubApp = ForgeGitHubAppConfig{AppID: 7, PrivateKey: testAppKeyPEM(t)}
	clock := &tallyClock{now: time.Date(2026, 9, 30, 14, 20, 0, 0, time.UTC)}
	var (
		reportsMu sync.Mutex
		reports   []string
	)
	tally := s.forgeHTTPClient().Transport.(countingTransport).tally
	tally.now, tally.start = clock.read, time.Date(2026, 9, 30, 13, 0, 0, 0, time.UTC)
	tally.report = func(format string, args ...any) {
		reportsMu.Lock()
		defer reportsMu.Unlock()
		reports = append(reports, fmt.Sprintf(format, args...))
	}

	conn := func(id int64) forge.Connection {
		return forge.Connection{
			ID: "c-" + strconv.FormatInt(id, 10), TenantID: "t1", Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp,
			Status: forge.StatusActive, ForgeBaseURL: f.srv.URL, Purpose: forge.PurposeRuntime, InstallationID: id,
		}
	}
	listRepos := func(at time.Time, c forge.Connection) {
		t.Helper()
		clock.set(at)
		admin, err := s.forgeAdminFor(context.Background(), c)
		if err != nil {
			t.Fatalf("installation %d: client: %v", c.InstallationID, err)
		}
		if _, err := admin.ListRepos(context.Background(), forge.RepoQuery{}); err != nil {
			t.Fatalf("installation %d: list repos: %v", c.InstallationID, err)
		}
	}
	listRepos(time.Date(2026, 9, 30, 14, 20, 0, 0, time.UTC), conn(101))
	listRepos(time.Date(2026, 9, 30, 14, 20, 10, 0, time.UTC), conn(202))
	listRepos(time.Date(2026, 9, 30, 14, 20, 20, 0, time.UTC), conn(101))
	listRepos(time.Date(2026, 9, 30, 14, 20, 30, 0, time.UTC), conn(202))

	clock.set(time.Date(2026, 9, 30, 15, 2, 0, 0, time.UTC))
	s.flushForgeTally()

	host := f.srv.Listener.Addr().String()
	wantCounts := "forge HTTP: 6 requests in the hour ending 2026-09-30T15:00Z — " +
		host + " rest other=2, " +
		host + " rest installation 101 other=2, " +
		host + " rest installation 202 other=2"
	wantBudget := "forge rate limit: lowest remaining in the hour ending 2026-09-30T15:00Z — " +
		host + " rest installation 101: 4989 of 5000 at 14:20:20Z (resets 14:45:00Z), " +
		host + " rest installation 202: 11949 of 12500 at 14:20:30Z (resets 14:58:00Z)"
	reportsMu.Lock()
	defer reportsMu.Unlock()
	if len(reports) != 2 || reports[0] != wantCounts || reports[1] != wantBudget {
		t.Fatalf("the hour reported\n%s\nwant exactly\n%s\n%s", strings.Join(reports, "\n"), wantCounts, wantBudget)
	}
}

// tallyAnswer is one forge request and its answer as the counting transport
// sees them: counted when sent, its budget read when answered.
type tallyAnswer struct {
	installation int64 // 0: sent with any other credential
	lane, url    string
	remaining    string
	limit        string
	reset        time.Time
	sent, seen   time.Time
}

func (a tallyAnswer) feed(t *testing.T, tally *forgeRequestTally, clock *tallyClock) {
	t.Helper()
	ctx := context.Background()
	if a.lane != "" {
		ctx = withForgeLane(ctx, a.lane)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.installation != 0 {
		// The label rides the context exactly as forge.InstallationClient
		// puts it there: through a transport the request passes.
		var labelled *http.Request
		_, _ = forge.InstallationClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			labelled = r
			return nil, fmt.Errorf("captured")
		})}, a.installation).Do(req)
		if labelled == nil {
			t.Fatal("the installation client sent nothing")
		}
		req = labelled
	}
	hdr := http.Header{}
	if a.remaining != "" {
		hdr.Set("X-RateLimit-Remaining", a.remaining)
	}
	if a.limit != "" {
		hdr.Set("X-RateLimit-Limit", a.limit)
	}
	if !a.reset.IsZero() {
		hdr.Set("X-RateLimit-Reset", strconv.FormatInt(a.reset.Unix(), 10))
	}
	clock.set(a.sent)
	tally.observe(req)
	clock.set(a.seen)
	tally.observeBudget(req, hdr)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// newTestTally is a tally on clock whose reports are collected.
func newTestTally(clock *tallyClock) (*forgeRequestTally, func() []string) {
	var (
		mu      sync.Mutex
		reports []string
	)
	tally := newForgeRequestTally(clock.read, func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		reports = append(reports, fmt.Sprintf(format, args...))
	})
	return tally, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), reports...)
	}
}

// Concurrent answers arrive in no order anyone chose, so the hour's lowest
// reading must not depend on it: equal remainings are told apart by when they
// were seen, then by their reset — never by which answer came in first. Fed in
// both orders, the line is the same.
func TestForgeRequestTally_TheLowestBudgetDoesNotDependOnArrivalOrder(t *testing.T) {
	at := func(m, s int) time.Time { return time.Date(2026, 9, 30, 14, m, s, 0, time.UTC) }
	answers := []tallyAnswer{
		{installation: 101, url: "https://api.github.com/repos/o/r/pulls/1", remaining: "900", limit: "5000", reset: at(50, 0), sent: at(10, 0), seen: at(10, 0)},
		{installation: 101, url: "https://api.github.com/repos/o/r/pulls/2", remaining: "700", limit: "5000", reset: at(50, 0), sent: at(20, 0), seen: at(20, 0)},
		// The same remaining seen later: the earlier sighting is the one kept.
		{installation: 101, url: "https://api.github.com/repos/o/r/pulls/3", remaining: "700", limit: "5000", reset: at(51, 0), sent: at(30, 0), seen: at(30, 0)},
		{installation: 202, url: "https://api.github.com/repos/o/r/pulls/4", remaining: "40", limit: "5000", reset: at(55, 0), sent: at(15, 0), seen: at(15, 0)},
		// Seen at the same instant: the earlier reset is the one kept.
		{installation: 202, url: "https://api.github.com/repos/o/r/pulls/5", remaining: "40", limit: "5000", reset: at(54, 0), sent: at(15, 0), seen: at(15, 0)},
	}
	run := func(order []tallyAnswer) []string {
		clock := &tallyClock{now: at(0, 0)}
		tally, reports := newTestTally(clock)
		for _, a := range order {
			a.feed(t, tally, clock)
		}
		clock.set(time.Date(2026, 9, 30, 15, 0, 1, 0, time.UTC))
		tally.flush()
		return reports()
	}
	reversed := make([]tallyAnswer, len(answers))
	for i, a := range answers {
		reversed[len(answers)-1-i] = a
	}
	forward, backward := run(answers), run(reversed)
	want := "forge rate limit: lowest remaining in the hour ending 2026-09-30T15:00Z — " +
		"api.github.com rest installation 101: 700 of 5000 at 14:20:00Z (resets 14:50:00Z), " +
		"api.github.com rest installation 202: 40 of 5000 at 14:15:00Z (resets 14:54:00Z)"
	for name, got := range map[string][]string{"in order": forward, "reversed": backward} {
		if len(got) != 2 || got[1] != want {
			t.Errorf("%s: reports = %q, want the budget line %q", name, got, want)
		}
	}
}

// A budget belongs to the installation, not to the lane that spent it: the
// sweep and the event path draw on one budget, so the hour reports one lowest
// reading for it while the count still splits by lane.
func TestForgeRequestTally_EveryLaneSpendsTheInstallationsOneBudget(t *testing.T) {
	at := func(m int) time.Time { return time.Date(2026, 9, 30, 14, m, 0, 0, time.UTC) }
	clock := &tallyClock{now: at(0)}
	tally, reports := newTestTally(clock)
	for _, a := range []tallyAnswer{
		{installation: 7, lane: forgeLaneGateSweeper, url: "https://api.github.com/repos/o/r/pulls/1", remaining: "300", limit: "5000", reset: at(59), sent: at(5), seen: at(5)},
		{installation: 7, url: "https://api.github.com/repos/o/r/issues", remaining: "250", limit: "5000", reset: at(59), sent: at(6), seen: at(6)},
		{installation: 7, lane: forgeLaneGateSweeper, url: "https://api.github.com/repos/o/r/pulls/1", remaining: "280", limit: "5000", reset: at(59), sent: at(7), seen: at(7)},
	} {
		a.feed(t, tally, clock)
	}
	clock.set(time.Date(2026, 9, 30, 15, 0, 1, 0, time.UTC))
	tally.flush()
	want := []string{
		"forge HTTP: 3 requests in the hour ending 2026-09-30T15:00Z — " +
			"api.github.com rest installation 7 merge-gate-sweeper=2, api.github.com rest installation 7 other=1",
		"forge rate limit: lowest remaining in the hour ending 2026-09-30T15:00Z — " +
			"api.github.com rest installation 7: 250 of 5000 at 14:06:00Z (resets 14:59:00Z)",
	}
	if got := reports(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("reports =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// An answer to any other credential reports a budget nobody can name — a PAT,
// a user token, the App's own JWT each spend their own — so it is counted and
// its budget left out, rather than folded into a minimum describing none of
// them. An answer with no remaining count reports no budget at all.
func TestForgeRequestTally_OnlyAnInstallationsTokenReportsABudget(t *testing.T) {
	at := func(m int) time.Time { return time.Date(2026, 9, 30, 14, m, 0, 0, time.UTC) }
	clock := &tallyClock{now: at(0)}
	tally, reports := newTestTally(clock)
	for _, a := range []tallyAnswer{
		{url: "https://api.github.com/user", remaining: "1", limit: "5000", reset: at(30), sent: at(1), seen: at(1)},
		{installation: 9, url: "https://api.github.com/repos/o/r/pulls/1", sent: at(2), seen: at(2)},
		{installation: 9, url: "https://api.github.com/repos/o/r/pulls/1", remaining: "soon", sent: at(3), seen: at(3)},
	} {
		a.feed(t, tally, clock)
	}
	clock.set(time.Date(2026, 9, 30, 15, 0, 1, 0, time.UTC))
	tally.flush()
	want := "forge HTTP: 3 requests in the hour ending 2026-09-30T15:00Z — " +
		"api.github.com rest installation 9 other=2, api.github.com rest other=1"
	if got := reports(); len(got) != 1 || got[0] != want {
		t.Errorf("reports = %q, want only the count %q", got, want)
	}

	// The same installation, now with a budget on its answer, next to the
	// other credential's: only the installation's is reported.
	clock.set(at(0).Add(time.Hour))
	(tallyAnswer{url: "https://api.github.com/user", remaining: "0", sent: at(61), seen: at(61)}).feed(t, tally, clock)
	(tallyAnswer{installation: 9, url: "https://api.github.com/graphql", remaining: "4999", sent: at(62), seen: at(62)}).feed(t, tally, clock)
	clock.set(time.Date(2026, 9, 30, 16, 0, 1, 0, time.UTC))
	tally.flush()
	wantBudget := "forge rate limit: lowest remaining in the hour ending 2026-09-30T16:00Z — api.github.com graphql installation 9: 4999 at 15:02:00Z"
	if got := reports(); len(got) != 3 || got[2] != wantBudget {
		t.Errorf("reports = %q, want a third %q — no limit or reset said, none printed", got, wantBudget)
	}
}

// An answer can arrive after the hour its request was counted in: it is a
// reading of the hour it arrives in, and that hour is reported for it even
// when no request was sent in it — turned by the next request like any other.
func TestForgeRequestTally_AnAnswerAfterTheHourTurnedIsReadInTheNextHour(t *testing.T) {
	clock := &tallyClock{now: time.Date(2026, 9, 30, 14, 0, 0, 0, time.UTC)}
	tally, reports := newTestTally(clock)
	(tallyAnswer{
		installation: 3, url: "https://api.github.com/repos/o/r/pulls/1", remaining: "12", limit: "5000",
		reset: time.Date(2026, 9, 30, 15, 30, 0, 0, time.UTC),
		sent:  time.Date(2026, 9, 30, 14, 59, 59, 0, time.UTC), seen: time.Date(2026, 9, 30, 15, 0, 2, 0, time.UTC),
	}).feed(t, tally, clock)
	(tallyAnswer{
		installation: 3, url: "https://api.github.com/repos/o/r/pulls/2",
		sent: time.Date(2026, 9, 30, 16, 5, 0, 0, time.UTC), seen: time.Date(2026, 9, 30, 16, 5, 0, 0, time.UTC),
	}).feed(t, tally, clock)
	clock.set(time.Date(2026, 9, 30, 16, 10, 0, 0, time.UTC))
	tally.flush()
	want := []string{
		"forge HTTP: 1 requests in the hour ending 2026-09-30T15:00Z — api.github.com rest installation 3 other=1",
		"forge rate limit: lowest remaining in the hour ending 2026-09-30T16:00Z — " +
			"api.github.com rest installation 3: 12 of 5000 at 15:00:02Z (resets 15:30:00Z)",
		"forge HTTP: 1 requests in the hour ending 2026-09-30T17:00Z (until 16:10:00Z, stopping) — api.github.com rest installation 3 other=1",
	}
	if got := reports(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("reports =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// The budget readings name hosts too, and a forge chooses where its redirects
// point: one hour names a bounded number of them, the same bound the count
// keeps, and a host already named keeps its own reading.
func TestForgeRequestTally_BoundsTheHostsOneHoursBudgetsName(t *testing.T) {
	clock := &tallyClock{now: time.Date(2026, 9, 30, 14, 0, 0, 0, time.UTC)}
	tally, _ := newTestTally(clock)
	at := time.Date(2026, 9, 30, 14, 1, 0, 0, time.UTC)
	(tallyAnswer{installation: 5, url: "https://api.github.com/repos/o/r/pulls/1", remaining: "4000", sent: at, seen: at}).feed(t, tally, clock)
	for i := 0; i < 3*forgeTallyMaxHosts; i++ {
		(tallyAnswer{installation: 5, url: fmt.Sprintf("https://ghe-%d.example/api/v3/repos/o/r", i), remaining: strconv.Itoa(100 + i), sent: at, seen: at}).feed(t, tally, clock)
	}
	(tallyAnswer{installation: 5, url: "https://api.github.com/repos/o/r/pulls/1", remaining: "3999", sent: at, seen: at}).feed(t, tally, clock)
	tally.mu.Lock()
	defer tally.mu.Unlock()
	hosts := map[string]bool{}
	for k := range tally.budgets {
		hosts[k.host] = true
	}
	if len(hosts) > forgeTallyMaxHosts+1 {
		t.Errorf("one hour's budgets name %d hosts, want at most %d", len(hosts), forgeTallyMaxHosts+1)
	}
	if b := tally.budgets[forgeBudgetKey{host: forgeTallyOtherHosts, api: "rest", installation: 5}]; b.remaining == 0 {
		t.Error("no reading was folded into the other-hosts entry")
	}
	if b := tally.budgets[forgeBudgetKey{host: "api.github.com", api: "rest", installation: 5}]; b.remaining != 3999 {
		t.Errorf("api.github.com's reading = %+v, want its own 3999 kept under its own host", b)
	}
}

// The incident's shape, through the forge client itself: a redirect hop, then
// the rate-limited 403 at zero. Every answer reports the budget, the refusal
// included, so the hour shows the installation ran dry and when.
func TestForgeHTTPClient_ReadsTheBudgetOnEveryAnswer(t *testing.T) {
	reset := time.Date(2026, 9, 30, 14, 40, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		if r.URL.Path == "/repos/o/r/pulls/1" {
			w.Header().Set("X-RateLimit-Remaining", "1")
			http.Redirect(w, r, "/repositories/42/pulls/1", http.StatusFound)
			return
		}
		w.Header().Set("X-RateLimit-Remaining", "0")
		http.Error(w, `{"message":"API rate limit exceeded for installation ID 77"}`, http.StatusForbidden)
	}))
	defer srv.Close()
	s := New(Config{}, iterlog.New(iterlog.LevelError, nil))
	clock := &tallyClock{now: time.Date(2026, 9, 30, 14, 21, 0, 0, time.UTC)}
	var (
		reportsMu sync.Mutex
		reports   []string
	)
	tally := s.forgeHTTPClient().Transport.(countingTransport).tally
	tally.now, tally.start = clock.read, time.Date(2026, 9, 30, 13, 0, 0, 0, time.UTC)
	tally.report = func(format string, args ...any) {
		reportsMu.Lock()
		defer reportsMu.Unlock()
		reports = append(reports, fmt.Sprintf(format, args...))
	}

	resp, err := forge.InstallationClient(s.forgeHTTPClient(), 77).Get(srv.URL + "/repos/o/r/pulls/1")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("ended on %d, want the 403 behind the redirect", resp.StatusCode)
	}
	clock.set(time.Date(2026, 9, 30, 15, 0, 1, 0, time.UTC))
	s.flushForgeTally()
	host := srv.Listener.Addr().String()
	want := []string{
		"forge HTTP: 2 requests in the hour ending 2026-09-30T15:00Z — " + host + " rest installation 77 other=2",
		"forge rate limit: lowest remaining in the hour ending 2026-09-30T15:00Z — " + host + " rest installation 77: 0 of 5000 at 14:21:00Z (resets 14:40:00Z)",
	}
	reportsMu.Lock()
	defer reportsMu.Unlock()
	if strings.Join(reports, "\n") != strings.Join(want, "\n") {
		t.Errorf("reports =\n%s\nwant\n%s", strings.Join(reports, "\n"), strings.Join(want, "\n"))
	}
}

// A github_app connection's sealed token is its installation's, so the org
// control proof that reads through it spends — and reports — that
// installation's budget, not an anonymous one.
func TestTeamControlsGitHubOrg_ChargesAnAppConnectionToItsInstallation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/user/memberships/orgs/acme" || r.Header.Get("Authorization") != "Bearer ghs_sealed" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-RateLimit-Remaining", "4321")
		w.Header().Set("X-RateLimit-Limit", "5000")
		http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
	}))
	defer srv.Close()
	s := New(Config{}, iterlog.New(iterlog.LevelError, nil))
	key := make([]byte, 32)
	sealer, err := secrets.NewAESGCMSealer(key)
	if err != nil {
		t.Fatal(err)
	}
	s.sealer = sealer
	sealed, err := forge.SealOAuthTokens(sealer, "c-app", "ghs_sealed", "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	conns := forge.NewMemoryConnectionStore()
	if err := conns.Create(context.Background(), forge.Connection{
		ID: "c-app", TenantID: "t1", Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp,
		Status: forge.StatusActive, ForgeBaseURL: srv.URL, InstallationID: 55, SealedPayload: sealed,
	}); err != nil {
		t.Fatal(err)
	}
	s.forgeConnections = conns
	s.forgeHTTPClient().Transport.(countingTransport).tally.now = func() time.Time {
		return time.Date(2026, 9, 30, 14, 30, 0, 0, time.UTC)
	}
	if s.teamControlsGitHubOrg(context.Background(), "t1", "acme") {
		t.Fatal("an installation token cannot read a user's org membership: the proof must not pass")
	}
	host := srv.Listener.Addr().String()
	s.forgeRequests.mu.Lock()
	defer s.forgeRequests.mu.Unlock()
	if got := s.forgeRequests.counts[forgeTallyKey{host: host, api: "rest", installation: 55}]; got != 1 {
		t.Errorf("requests charged to installation 55 = %d, want 1; counts: %v", got, s.forgeRequests.counts)
	}
	if b, ok := s.forgeRequests.budgets[forgeBudgetKey{host: host, api: "rest", installation: 55}]; !ok || b.remaining != 4321 {
		t.Errorf("installation 55's budget = %+v (recorded %v), want its 4321 remaining", b, ok)
	}
}
