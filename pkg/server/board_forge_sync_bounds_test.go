package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// What BOUNDS the incremental issue read, and what the bounds cost.
//
// The read's verdict decides whether the caller's high-water mark moves, and
// the mark is the only way back to an issue: every probe here is about a
// bound that could be mistaken for "the window is done" — a page the provider
// shortened, a page budget spent, a term that ended — or about the price the
// bound charges: forge requests on a lane, cards rewritten, a document
// replaced.

// clampingIssueClient serves at most limit rows per page whatever PerPage
// asks for — a Gitea/Forgejo instance's server-side [api] MAX_RESPONSE_ITEMS.
type clampingIssueClient struct {
	mu     sync.Mutex
	issues []forge.IssueRef
	limit  int
	calls  int
}

func (c *clampingIssueClient) ListIssues(_ context.Context, _ string, opts forge.IssueListOptions) ([]forge.IssueRef, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	per := opts.PerPage
	if per <= 0 || per > c.limit {
		per = c.limit
	}
	page := max(opts.Page, 1)
	start := (page - 1) * per
	if start >= len(c.issues) {
		return nil, nil
	}
	return c.issues[start:min(start+per, len(c.issues))], nil
}

func (c *clampingIssueClient) GetIssue(context.Context, string, int) (forge.IssueRef, error) {
	return forge.IssueRef{}, errors.New("unused")
}

func (c *clampingIssueClient) CreateIssue(context.Context, string, forge.NewIssue) (forge.IssueRef, error) {
	return forge.IssueRef{}, errors.New("unused")
}

func (c *clampingIssueClient) UpdateIssue(context.Context, string, int, forge.IssuePatch) (forge.IssueRef, error) {
	return forge.IssueRef{}, errors.New("unused")
}

func (c *clampingIssueClient) CommentIssue(context.Context, string, int, string) (forge.CommentRef, error) {
	return forge.CommentRef{}, errors.New("unused")
}

// A window of 250 served 50 rows at a time, whatever the read asks for: the
// completeness verdict must not rest on the page SIZE a provider chose to
// serve, because nothing in the seam promises the size asked for.
func TestSyncForgeIssuesToBoard_AClampedPageIsNotAnExhaustedWindow(t *testing.T) {
	board := newTestBoard(t)
	ic := &clampingIssueClient{issues: testIssues(250), limit: 50}

	_, created, _, err := syncForgeIssuesToBoard(
		context.Background(), ic, forge.ProviderForgejo, "conn1", "org/api", board, time.Time{}, nil)
	all, _ := board.List(native.ListFilter{})
	t.Logf("OBSERVED err=%v created=%d calls=%d board=%d of 250", err, created, ic.calls, len(all))
	if err == nil && (created != 250 || len(all) != 250) {
		t.Fatalf("a COMPLETE verdict (nil) over %d of 250 issues: the caller moves its mark and the rest is unreachable for good", created)
	}
}

// fakeGitea serves GET /api/v1/repos/{owner}/{repo}/issues the way a
// Forgejo/Gitea instance does — `since`/`page`/`limit` honoured, with `limit`
// clamped to maxItems ([api] MAX_RESPONSE_ITEMS, 50 by default).
type fakeGitea struct {
	mu     sync.Mutex
	pages  int
	limits []string
	srv    *httptest.Server
}

func newFakeGitea(t *testing.T, total, maxItems int) *fakeGitea {
	t.Helper()
	f := &fakeGitea{}
	born := time.Now().UTC()
	at := func(int) time.Time { return born }
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		nums, _, err := forgeIssueWindow(r, total, maxItems, at)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		f.mu.Lock()
		f.pages++
		f.limits = append(f.limits, r.URL.Query().Get("limit"))
		f.mu.Unlock()
		out := []map[string]any{}
		for _, i := range nums {
			out = append(out, forgeIssueEntry(i, at))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitea) spent() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pages, append([]string(nil), f.limits...)
}

// seedForgejoSyncIntegration is seedSyncIntegration's Forgejo twin: the REAL
// pkg/forge/forgejo client, pointed at a fake instance.
func seedForgejoSyncIntegration(t *testing.T, baseURL string, board native.BoardStore, mark time.Time) (*Server, forge.RepoIntegration) {
	t.Helper()
	s := newForgeTestServer(t)
	conn := forge.Connection{
		ID: "c-fj", TenantID: "t1", Provider: forge.ProviderForgejo, Kind: forge.KindPAT,
		Status: forge.StatusActive, ForgeBaseURL: baseURL, Purpose: forge.PurposeRuntime, CreatedAt: time.Now(),
	}
	sealed, err := forge.SealPAT(s.sealer, conn.ID, "fj-token")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	conn.SealedPayload = sealed
	if err := s.forgeConnections.Create(context.Background(), conn); err != nil {
		t.Fatalf("create connection: %v", err)
	}
	ri := forge.RepoIntegration{
		ID: "i-fj", TenantID: "t1", ConnectionID: conn.ID, RepoFullName: "org/api",
		SyncIssuesEnabled: true, LastSyncedAt: mark, CreatedAt: time.Now(),
	}
	if err := s.forgeIntegrations.Create(context.Background(), ri); err != nil {
		t.Fatalf("create integration: %v", err)
	}
	s.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
	return s, ri
}

// The same verdict end to end through the REAL Forgejo client, against an
// instance clamping `limit` to its own MAX_RESPONSE_ITEMS: a mark advanced
// here writes off every issue past the first page, and `since` is the only way
// back to them.
func TestSyncOneIntegration_AForgejoPageClampDoesNotMoveTheMark(t *testing.T) {
	fj := newFakeGitea(t, 250, 50)
	mark := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	board := newTestBoard(t)
	s, ri := seedForgejoSyncIntegration(t, fj.srv.URL, board, mark)

	_, created, _, err := s.syncOneIntegration(context.Background(), "t1", ri)
	pages, limits := fj.spent()
	all, _ := board.List(native.ListFilter{})
	got := storedMark(t, s, ri.ID)
	t.Logf("OBSERVED err=%v created=%d pages=%d limits=%v board=%d of 250 mark moved=%v",
		err, created, pages, limits, len(all), !got.Equal(mark))
	if err == nil && len(all) != 250 {
		t.Fatalf("the instance served %d of 250 issues and the sync reported success", len(all))
	}
	if !got.Equal(mark) && len(all) != 250 {
		t.Fatalf("mark moved to %s over %d unread issues — `since` is the only way back to them", got, 250-len(all))
	}
}

// The truncation verdict must survive a refused card: both hold the mark, but
// only the read verdict names the budget that cut this repo's window, which is
// the operator's one cue about what to raise.
func TestSyncForgeIssuesToBoard_TruncationSurvivesARefusedCard(t *testing.T) {
	board := &refusingBoard{BoardStore: newTestBoard(t), err: errors.New("boardmongo: create issue: i/o timeout")}
	ic := &pagedIssueClient{fullPages: true}

	_, _, _, err := syncForgeIssuesToBoard(
		context.Background(), ic, forge.ProviderGitHub, "conn1", "org/api", board, time.Time{}, nil)
	t.Logf("OBSERVED err=%v", err)
	if err == nil {
		t.Fatal("both failures vanished")
	}
	if !strings.Contains(err.Error(), "i/o timeout") {
		t.Errorf("error = %v, want the refused card named", err)
	}
	if !strings.Contains(err.Error(), "entries read in "+strconv.Itoa(forgeIssueSyncMaxPages)+" pages") {
		t.Fatalf("error = %v, want the truncated window named too — it is the operator's only cue that this repo's window is permanently cut", err)
	}
}

// The budget-exhausted refusal is ONE message serving two entry points with
// different powers: the cloud sweep, which keeps a high-water mark and will
// retry the window, and `iterion issue import`, which keeps none — it never
// calls StampSynced, there is no next pass, and there is no webhook fast path
// to raise a budget on. A message promising a retry from the same mark sends a
// self-hosted operator away waiting for a pass that will never come, when the
// remedy in their hands is a narrower --since or a raised budget. So the core
// states only what holds on BOTH paths, and the mark's fate is named by the
// caller that keeps one (see TestSyncOneIntegration_FailedUpsertHoldsTheMark).
func TestImportForgeIssues_ARefusedWindowPromisesNoMarkItDoesNotKeep(t *testing.T) {
	const perPage, budget = 3, 4 // a window of 100 000 entries served 3 at a time: four pages cannot exhaust it
	f := newFakeGitea(t, 100_000, perPage)
	board := newTestBoard(t)

	mirrored, created, updated, err := ImportForgeIssues(withForgeIssuePageBudget(context.Background(), budget),
		forge.ProviderForgejo, f.srv.URL, "tok", "org/api", board, time.Time{}, "")
	pages, _ := f.spent()
	cards, lerr := board.List(native.ListFilter{})
	if lerr != nil {
		t.Fatalf("list board: %v", lerr)
	}
	t.Logf("OBSERVED pages=%d mirrored=%d created=%d updated=%d cards=%d err=%v", pages, mirrored, created, updated, len(cards), err)
	if err == nil {
		t.Fatal("a window four pages could not exhaust was reported as complete — the import would exit 0 on a partial board")
	}
	if !strings.Contains(err.Error(), "entries read in "+strconv.Itoa(budget)+" pages") {
		t.Errorf("error = %v, want the budget that cut the window named — it is the operator's cue about what to raise", err)
	}
	for _, claim := range []string{"high-water mark", "next pass", "webhook"} {
		if strings.Contains(err.Error(), claim) {
			t.Errorf("the import's refusal claims %q, which this entry point has none of: %v", claim, err)
		}
	}
	// The pages that DID arrive are on the board: the refusal is about the
	// REST of the window, which is what makes the counts worth reporting.
	if len(cards) != pages*perPage || mirrored != len(cards) {
		t.Errorf("%d pages of %d served, %d cards on the board, mirrored=%d — the refusal must be about the rest of the window, not about these",
			pages, perPage, len(cards), mirrored)
	}
}

// The page budget bounds ENTRIES: the issues endpoint serves pull requests
// alongside issues on GitHub and Forgejo, and a refusal that reports 20 000
// discarded PRs as issues sends an operator after a repo that has none.
func TestSyncForgeIssuesToBoard_ThePageBudgetCountsEntries(t *testing.T) {
	board := &countingBoard{}
	ic := &prIssueClient{}

	_, created, _, err := syncForgeIssuesToBoard(
		context.Background(), ic, forge.ProviderGitHub, "conn1", "org/api", board, time.Time{}, nil)
	t.Logf("OBSERVED created=%d err=%v", created, err)
	if err == nil {
		t.Fatal("a window the budget could not exhaust must surface")
	}
	if created != 0 {
		t.Fatalf("created=%d, want 0 — every entry was a pull request", created)
	}
	if strings.Contains(err.Error(), "issues read") {
		t.Fatalf("the refusal reports %d pull requests as issues: %v", forgeIssueSyncMaxPages*forgeIssueSyncPerPage, err)
	}
}

// The two counts of ONE refusal are over two different sets, and the refusal
// is one line on one operator's screen (the sweep logs it through a single
// Warn): the read counts ENTRIES, pull requests included because that endpoint
// serves them, while the board's refusal counts the ISSUES it was offered —
// the PRs are discarded before the upsert. Each has to name its own set, or
// "100 of 100 refused" beside "200 read" is a contradiction nothing on screen
// can resolve.
func TestSyncForgeIssuesToBoard_TheRefusalCountsIssuesWhereTheReadCountsEntries(t *testing.T) {
	const budget = 2
	board := &refusingBoard{BoardStore: newTestBoard(t), err: errors.New("boardmongo: create issue: i/o timeout")}
	ic := &prIssueClient{withIssues: true} // every second entry a pull request

	mirrored, _, _, err := syncForgeIssuesToBoard(withForgeIssuePageBudget(context.Background(), budget),
		ic, forge.ProviderGitHub, "conn1", "org/api", board, time.Time{}, nil)
	t.Logf("OBSERVED mirrored=%d err=%v", mirrored, err)
	if err == nil {
		t.Fatal("a window the budget could not exhaust, on a board refusing every card, was reported as mirrored whole")
	}
	entries := budget * forgeIssueSyncPerPage
	if want := strconv.Itoa(entries) + " entries read in " + strconv.Itoa(budget) + " pages"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want the read priced in %q — the premise is a window of mixed entries", err, want)
	}
	// Half the window was an issue, and the board took none of them.
	want := strconv.Itoa(entries/2) + " of " + strconv.Itoa(entries/2) + " issues refused"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v, want %q: this denominator counts only what reached the board, where the read's own count "+
			"above it counts the pull requests too — one word for two sets leaves the operator two numbers that cannot both be true", err, want)
	}
}

// The remedy a refusal offers has to be a power of the caller it reaches. The
// page budget is a rebuild for all four callers and says so; narrowing the
// window is `iterion issue import --since` alone — the three cloud readers
// take theirs from the integration's high-water mark, which no operator
// surface writes (the only PATCH on an integration carries
// SyncIssuesEnabled). So the shared core states the budget, and the import
// adds its own clause.
func TestForgeIssueWindowTruncated_TheRemedyBelongsToTheCallerThatHasIt(t *testing.T) {
	// The cloud reader, from its real entry point: the leased sweep's
	// per-integration pass, whose window comes from the stored mark.
	gh := newFakeGitHubIssues(t, 250, 0, 0)
	mark := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	s, ri := seedSyncIntegration(t, gh.srv.URL, newTestBoard(t), mark)
	_, _, _, cloudErr := s.syncOneIntegration(withForgeIssuePageBudget(context.Background(), 2), "t1", ri)
	t.Logf("OBSERVED cloud err=%v", cloudErr)
	if cloudErr == nil {
		t.Fatal("a window two pages could not exhaust was reported as complete")
	}
	if !strings.Contains(cloudErr.Error(), "raise the page budget") {
		t.Errorf("error = %v, want the remedy both callers have", cloudErr)
	}
	for _, claim := range []string{"narrow the window", "--since"} {
		if strings.Contains(cloudErr.Error(), claim) {
			t.Errorf("the cloud refusal offers %q, which this caller has no surface for — its window is the stored mark: %v", claim, cloudErr)
		}
	}

	// The import, which owns its window: same core, one more clause.
	f := newFakeGitea(t, 100_000, 3)
	_, _, _, importErr := ImportForgeIssues(withForgeIssuePageBudget(context.Background(), 4),
		forge.ProviderForgejo, f.srv.URL, "tok", "org/api", newTestBoard(t), time.Time{}, "")
	t.Logf("OBSERVED import err=%v", importErr)
	if importErr == nil {
		t.Fatal("a window four pages could not exhaust was reported as complete")
	}
	if !strings.Contains(importErr.Error(), "narrow the window with --since") {
		t.Errorf("error = %v, want the remedy this entry point HAS named: --since is its own argument", importErr)
	}
}

// prIssueClient answers full pages of PULL REQUESTS — the entries the sync
// then discards, on an endpoint that conflates them with issues.
type prIssueClient struct {
	mu    sync.Mutex
	calls int
	// withIssues interleaves real issues one for one, the window shape that
	// makes the entry count and the issue count of one refusal differ.
	withIssues bool
}

func (p *prIssueClient) ListIssues(_ context.Context, _ string, opts forge.IssueListOptions) ([]forge.IssueRef, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	per := max(opts.PerPage, 1)
	page := max(opts.Page, 1)
	out := make([]forge.IssueRef, 0, per)
	for i := range per {
		n := (page-1)*per + i + 1
		out = append(out, forge.IssueRef{
			Number: n, Title: "entry " + strconv.Itoa(n), State: "open",
			IsPullRequest: !p.withIssues || n%2 == 1,
		})
	}
	return out, nil
}

func (p *prIssueClient) GetIssue(context.Context, string, int) (forge.IssueRef, error) {
	return forge.IssueRef{}, errors.New("unused")
}

func (p *prIssueClient) CreateIssue(context.Context, string, forge.NewIssue) (forge.IssueRef, error) {
	return forge.IssueRef{}, errors.New("unused")
}

func (p *prIssueClient) UpdateIssue(context.Context, string, int, forge.IssuePatch) (forge.IssueRef, error) {
	return forge.IssueRef{}, errors.New("unused")
}

func (p *prIssueClient) CommentIssue(context.Context, string, int, string) (forge.CommentRef, error) {
	return forge.CommentRef{}, errors.New("unused")
}

// The sweep's forge requests must be charged to their own lane: its per-pass
// request count is what the lease exists to protect, and the hourly tally is
// where an operator reads it.
func TestBoardSyncPasses_SpendOnItsOwnForgeLane(t *testing.T) {
	gh := newFakeGitHubIssues(t, 3, 0, 0)
	s, _ := seedSyncIntegration(t, gh.srv.URL, newTestBoard(t), time.Now().UTC().Add(-time.Hour))
	s.logger = iterlog.New(iterlog.LevelInfo, &lockedBuffer{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.boardSyncPasses(ctx, time.Hour) // one opening pass
	}()
	waitForCond(t, 5*time.Second, "the opening pass", func() bool {
		_, pages := gh.served()
		return pages > 0
	})
	cancel()
	<-done

	lanes := laneTally(t, s)
	t.Logf("OBSERVED lanes=%v", lanes)
	if lanes[forgeLaneBoardIssueSync] == 0 {
		t.Fatalf("the sweep's reads are charged to %v, not to its own lane — the budget the lease protects cannot be attributed to it", lanes)
	}
}

// An ended term must stop the sweep where it is. The lease makes a cancelled
// ctx mid-pass routine — a step-down, a rollout — and the paged read makes a
// pass long enough for one to land inside it.
func TestSyncAllEnabledIntegrations_AnEndedTermStopsTheSweep(t *testing.T) {
	log := &lockedBuffer{}
	s := newForgeTestServer(t)
	s.logger = iterlog.New(iterlog.LevelInfo, log)
	for i := range 12 {
		ri := forge.RepoIntegration{
			ID: fmt.Sprintf("i-%d", i), TenantID: "t1", ConnectionID: "c-gone",
			RepoFullName: fmt.Sprintf("org/repo-%d", i), SyncIssuesEnabled: true, CreatedAt: time.Now(),
		}
		if err := s.forgeIntegrations.Create(context.Background(), ri); err != nil {
			t.Fatalf("create integration: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the term ended: a step-down, a rollout, a SIGTERM

	s.syncAllEnabledIntegrations(ctx)
	warns := strings.Count(log.String(), "board sync: t1/org/repo-")
	t.Logf("OBSERVED per-repo warn lines after the term ended = %d", warns)
	if warns != 0 {
		t.Fatalf("the sweep walked %d repos after its term ended", warns)
	}
}

// An identical pass must not rewrite every card. Holding the mark on an
// incomplete read means re-reading the same window every sweep; the accepted
// cost of that is the READ, not a board write and an issue_updated event per
// card per sweep, for as long as the mark is held.
func TestSyncForgeIssuesToBoard_AnUnchangedCardIsNotRewritten(t *testing.T) {
	board := newTestBoard(t)
	run := func() (int, int) {
		_, c, u, err := syncForgeIssuesToBoard(
			context.Background(), &pagedIssueClient{issues: testIssues(3)},
			forge.ProviderGitHub, "conn1", "org/api", board, time.Time{}, nil)
		if err != nil {
			t.Fatalf("sync: %v", err)
		}
		return c, u
	}
	c1, u1 := run()
	cardID := forgeCardID(forge.ProviderGitHub, "org/api", 1)
	first, err := board.Get(cardID)
	if err != nil {
		t.Fatalf("get card: %v", err)
	}
	writtenAt := first.UpdatedAt

	c2, u2 := run()
	again, err := board.Get(cardID)
	if err != nil {
		t.Fatalf("get card: %v", err)
	}
	// The write is not the whole cost: native.Store.Update emits an
	// issue_updated event the trigger spine consumes as `card.updated`.
	updates := 0
	_ = board.ScanEvents(func(e *native.Event) bool {
		if e.Type == native.EvtIssueUpdated {
			updates++
		}
		return true
	})
	t.Logf("OBSERVED pass1 created=%d updated=%d | pass2 created=%d updated=%d | card rewritten=%v | issue_updated events=%d",
		c1, u1, c2, u2, !again.UpdatedAt.Equal(writtenAt), updates)
	if u2 != 0 {
		t.Errorf("pass 2 reported %d updates over an unchanged window, want 0", u2)
	}
	if !again.UpdatedAt.Equal(writtenAt) {
		t.Errorf("the card was rewritten (%s → %s) though nothing about it changed — a held mark re-reads its whole window every pass",
			writtenAt, again.UpdatedAt)
	}
	if updates != 0 {
		t.Fatalf("%d issue_updated events over two passes of an unchanged window, want 0 — that is one card event per card per sweep, for as long as the mark is held", updates)
	}

	// And a forge change still lands: the skip is "nothing changed", not
	// "never write again".
	changed := testIssues(3)
	changed[0].URL = "https://forge.example/org/api/issues/1"
	_, c3, u3, err := syncForgeIssuesToBoard(
		context.Background(), &pagedIssueClient{issues: changed},
		forge.ProviderGitHub, "conn1", "org/api", board, time.Time{}, nil)
	if err != nil {
		t.Fatalf("third sync: %v", err)
	}
	stamped, err := board.Get(cardID)
	if err != nil {
		t.Fatalf("get card: %v", err)
	}
	if c3 != 0 || u3 != 1 || stamped.External == nil || stamped.External.URL != changed[0].URL {
		t.Fatalf("a changed forge URL synced as created=%d updated=%d and left the card's linkage at %+v",
			c3, u3, stamped.External)
	}
}

// Stamping the mark must not revert what an operator changed during the pass.
// The integration this call is handed is a snapshot read before the window
// was, and a paged pass is long enough for a patch to land inside it.
func TestSyncOneIntegration_TheMarkStampKeepsAConcurrentPatch(t *testing.T) {
	var s *Server
	var ri forge.RepoIntegration
	gh := newFakeGitHubIssuesHooked(t, 250, func(page int) {
		if page != 2 {
			return
		}
		// Exactly what handlePatchForgeIntegration does: read, flip, Update.
		cur, err := s.forgeIntegrations.Get(context.Background(), ri.ID)
		if err != nil {
			t.Errorf("operator read: %v", err)
			return
		}
		cur.SyncIssuesEnabled = false
		if err := s.forgeIntegrations.Update(context.Background(), cur); err != nil {
			t.Errorf("operator patch: %v", err)
		}
	})
	mark := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	s, ri = seedSyncIntegration(t, gh.srv.URL, newTestBoard(t), mark)

	if _, _, _, err := s.syncOneIntegration(context.Background(), "t1", ri); err != nil {
		t.Fatalf("sync: %v", err)
	}
	after, err := s.forgeIntegrations.Get(context.Background(), ri.ID)
	if err != nil {
		t.Fatalf("get integration: %v", err)
	}
	t.Logf("OBSERVED sync_issues_enabled=%v mark moved=%v", after.SyncIssuesEnabled, !after.LastSyncedAt.Equal(mark))
	if after.SyncIssuesEnabled {
		t.Fatalf("the pass reverted the operator's patch: the repo is still syncing after it was turned off")
	}
	if !after.LastSyncedAt.After(mark) {
		t.Fatalf("mark = %s, want it advanced after a complete read", after.LastSyncedAt)
	}
}

// fakeGitHubIssuesHooked is newFakeGitHubIssues with a per-page hook, so a
// test can act — as an operator would — WHILE the paged read runs.
type fakeGitHubIssuesHooked struct {
	srv *httptest.Server
	mu  sync.Mutex
	n   int
}

func newFakeGitHubIssuesHooked(t *testing.T, total int, onPage func(page int)) *fakeGitHubIssuesHooked {
	t.Helper()
	f := &fakeGitHubIssuesHooked{}
	born := time.Now().UTC()
	at := func(int) time.Time { return born }
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		nums, _, err := forgeIssueWindow(r, total, 0, at)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		f.mu.Lock()
		f.n++
		f.mu.Unlock()
		if onPage != nil {
			onPage(max(page, 1))
		}
		out := []map[string]any{}
		for _, i := range nums {
			out = append(out, forgeIssueEntry(i, at))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHubIssuesHooked) pages() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

// The webhook fast path must bound its own read: it runs on every replica
// under no lease, inside a 30 s ceiling sized for a single call, so a wide
// window would otherwise cost every delivery whatever requests fit that
// ceiling — and mark nothing, since an incomplete read holds the mark.
func TestProjectForgeWebhookToBoard_BoundsItsRead(t *testing.T) {
	gh := newFakeGitHubIssuesHooked(t, 5000, nil) // 50 pages of 100
	mark := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	s, ri := seedSyncIntegration(t, gh.srv.URL, &countingBoard{}, mark)
	s.logger = iterlog.New(iterlog.LevelInfo, &lockedBuffer{})

	s.projectForgeWebhookToBoard(context.Background(), ri.RepoFullName)
	spent := gh.pages()
	got := storedMark(t, s, ri.ID)
	t.Logf("OBSERVED fast-path pages spent=%d mark moved=%v", spent, !got.Equal(mark))
	if spent > forgeIssueSyncFastPathMaxPages {
		t.Fatalf("one webhook delivery spent %d forge requests on a wide window, want at most %d — every delivery, on every replica",
			spent, forgeIssueSyncFastPathMaxPages)
	}
	if !got.Equal(mark) {
		t.Fatalf("the fast path moved the mark over a window it only partly read")
	}
}

// And the bound must not refuse the legitimate: a normal window completes on
// the fast path, lands whole and advances the mark — which is what keeps a
// live issue's card fresh within seconds.
func TestProjectForgeWebhookToBoard_SyncsANormalWindowWhole(t *testing.T) {
	gh := newFakeGitHubIssuesHooked(t, 250, nil)
	mark := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	board := newTestBoard(t)
	s, ri := seedSyncIntegration(t, gh.srv.URL, board, mark)
	s.logger = iterlog.New(iterlog.LevelInfo, &lockedBuffer{})

	s.projectForgeWebhookToBoard(context.Background(), ri.RepoFullName)
	all, _ := board.List(native.ListFilter{})
	got := storedMark(t, s, ri.ID)
	lanes := laneTally(t, s)
	t.Logf("OBSERVED board=%d pages=%d mark moved=%v lanes=%v", len(all), gh.pages(), !got.Equal(mark), lanes)
	if len(all) != 250 || !got.After(mark) {
		t.Fatalf("board holds %d of 250 cards and the mark moved=%v: the fast path must still mirror a normal window whole",
			len(all), !got.Equal(mark))
	}
	// And on its own lane: this read is paid per delivery and per replica,
	// which the leased sweep's line does not measure.
	if lanes[forgeLaneBoardWebhookProjection] == 0 {
		t.Fatalf("the projection's reads are charged to %v, not to its own lane", lanes)
	}
}

// laneTally reads the hourly forge tally as lane → requests.
func laneTally(t *testing.T, s *Server) map[string]int64 {
	t.Helper()
	if s.forgeRequests == nil {
		t.Fatal("no tally: nothing went through the counted forge client")
	}
	s.forgeRequests.mu.Lock()
	defer s.forgeRequests.mu.Unlock()
	lanes := map[string]int64{}
	for k, n := range s.forgeRequests.counts {
		lanes[k.lane] += n
	}
	return lanes
}

// Every field of the forge linkage participates in the comparison that decides
// whether a card is rewritten: one left out is a forge change that never
// reaches the board — a card whose URL, state or author stays wrong for good,
// since the sync only ever re-reads what the forge reports as updated.
func TestSameForgeRef_EveryFieldParticipates(t *testing.T) {
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	base := native.ExternalRef{
		Provider: "github", ConnectionID: "c1", Repo: "org/api", Number: 7,
		URL: "https://forge.example/org/api/issues/7", State: "open", Author: "alice",
		Project: &native.ExternalProject{Owner: "SocialGouv", Number: 203, ItemID: "PVTI_1", Status: "Planned", StatusAt: at, StateAt: at},
	}
	if !sameForgeRef(&base, &base) {
		t.Fatal("an identical linkage must compare equal, or every card is rewritten every pass")
	}
	for name, mutate := range map[string]func(*native.ExternalRef){
		"provider":      func(r *native.ExternalRef) { r.Provider = "forgejo" },
		"connection_id": func(r *native.ExternalRef) { r.ConnectionID = "c2" },
		"repo":          func(r *native.ExternalRef) { r.Repo = "org/other" },
		"number":        func(r *native.ExternalRef) { r.Number = 8 },
		"url":           func(r *native.ExternalRef) { r.URL = "https://forge.example/org/api/issues/8" },
		"state":         func(r *native.ExternalRef) { r.State = "closed" },
		"author":        func(r *native.ExternalRef) { r.Author = "bob" },
		"project":       func(r *native.ExternalRef) { p := *r.Project; p.Status = "Done"; r.Project = &p },
		"project_unset": func(r *native.ExternalRef) { r.Project = nil },
	} {
		t.Run(name, func(t *testing.T) {
			other := base
			mutate(&other)
			if sameForgeRef(&base, &other) {
				t.Errorf("%s is not compared — a forge change to it would never be written to the card", name)
			}
		})
	}
}

// And the comparison must compare INSTANTS, not their representations: a
// time.Time carries a monotonic reading and a location, so the same instant
// read back from another source is routinely unequal under ==. Under == every
// card carrying project sync state would be rewritten on every pass — the
// churn the comparison exists to stop.
func TestSameForgeRef_ComparesInstantsNotRepresentations(t *testing.T) {
	utc := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	paris := utc.In(time.FixedZone("CEST", 2*60*60))
	a := native.ExternalRef{Provider: "github", Repo: "org/api", Number: 7,
		Project: &native.ExternalProject{Status: "Planned", StatusAt: utc, StateAt: utc}}
	b := native.ExternalRef{Provider: "github", Repo: "org/api", Number: 7,
		Project: &native.ExternalProject{Status: "Planned", StatusAt: paris, StateAt: paris}}
	if !sameForgeRef(&a, &b) {
		t.Error("the same instant in another location must compare equal")
	}
}

// A card with no linkage yet is never "already what we would write": the FIRST
// sync of a card created by hand or by push-to-forge has to stamp it.
func TestSameForgeRef_AnUnlinkedCardIsNeverEqual(t *testing.T) {
	ext := &native.ExternalRef{Provider: "github", Repo: "org/api", Number: 7}
	if sameForgeRef(ext, nil) {
		t.Error("a card with no forge linkage must compare unequal, or it is never stamped")
	}
}

// authoredGitHub serves the TWO endpoints a sweep of a real window touches:
// the issues collection, one page at a time, each entry carrying its own
// author login, and the collaborator-permission read the author-trust gate
// falls back on for that login. It counts them apart, so a test reads what a
// pass SPENDS and not only how many pages it asked for — the other fakes here
// serve author-less entries, where the trust path returns before any request
// and the whole cost of the stamp is invisible.
type authoredGitHub struct {
	mu       sync.Mutex
	pageReqs int
	permReqs int
	srv      *httptest.Server
}

func newAuthoredGitHub(t *testing.T, total int) *authoredGitHub {
	t.Helper()
	f := &authoredGitHub{}
	born := time.Now().UTC()
	at := func(int) time.Time { return born }
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		nums, _, err := forgeIssueWindow(r, total, 0, at)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		f.mu.Lock()
		f.pageReqs++
		f.mu.Unlock()
		out := []map[string]any{}
		for _, i := range nums {
			e := forgeIssueEntry(i, at)
			// One distinct author per issue — what a public repo's drive-by
			// history looks like, and the case the trust gate exists for.
			e["user"] = map[string]any{"login": fmt.Sprintf("author-%d", i)}
			out = append(out, e)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/collaborators/{login}/permission", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.permReqs++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"permission":"write"}`))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *authoredGitHub) spent() (pages, perms int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pageReqs, f.permReqs
}

// What a pass over an ALREADY MIRRORED window costs the installation's
// request budget, end to end through the real GitHub client. Holding a mark
// re-reads its window on every sweep, and the price of that must be the READ:
// the trust verdict is read by one arm — a fresh open card's stamp — so a
// pass that writes nothing must buy no permission request at all. Resolving
// it per entry instead turns a 250-entry window into 254 requests a sweep,
// forever, for one repo, against an installation's 5 000/hour.
func TestSyncOneIntegration_AMirroredWindowCostsNoTrustRequest(t *testing.T) {
	gh := newAuthoredGitHub(t, 250)
	mark := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	board := newTestBoard(t)
	s, ri := seedSyncIntegration(t, gh.srv.URL, board, mark)
	s.logger = iterlog.New(iterlog.LevelInfo, &lockedBuffer{})

	mirrored, created, _, err := s.syncOneIntegration(context.Background(), "t1", ri)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	pages1, perms1 := gh.spent()
	t.Logf("OBSERVED first pass: mirrored=%d created=%d page requests=%d permission requests=%d",
		mirrored, created, pages1, perms1)
	// The irreducible remainder: one permission read per distinct author of a
	// card this pass created OPEN, because that card carries the stamp.
	if created != 250 || perms1 != created {
		t.Fatalf("first pass created=%d with %d permission requests, want one per created card", created, perms1)
	}

	// The gate caches a verdict for authorTrustDefaultTTL (10 min) — longer
	// than one sweep interval, shorter than two — so a sweep does re-resolve
	// the window it re-reads. Expiring the cache here is what the clock does
	// between two sweeps.
	gate := s.authorTrustGate()
	gate.mu.Lock()
	gate.cache = map[string]authorTrustEntry{}
	gate.mu.Unlock()

	// The same window again, nothing changed on the forge: the mark in `ri`
	// is the one the first pass was handed, exactly as a held mark would be.
	mirrored2, c2, u2, err := s.syncOneIntegration(context.Background(), "t1", ri)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	pages2, perms2 := gh.spent()
	t.Logf("OBSERVED second pass over an UNCHANGED window: mirrored=%d created=%d updated=%d page requests=%d permission requests=%d",
		mirrored2, c2, u2, pages2-pages1, perms2-perms1)
	if c2 != 0 || u2 != 0 {
		t.Fatalf("second pass wrote created=%d updated=%d over an unchanged window, want 0/0", c2, u2)
	}
	if pages2-pages1 == 0 {
		t.Fatal("the second pass read nothing: this witness measures the cost of a window that IS re-read")
	}
	if got := perms2 - perms1; got != 0 {
		t.Fatalf("a pass that wrote no card spent %d collaborator-permission requests on %d entries — "+
			"one per entry of a window the page budget lets reach %d, every sweep, for a value no existing card reads",
			got, mirrored2, forgeIssueSyncMaxPages*forgeIssueSyncPerPage)
	}
	// And the pass still reports the window it mirrored, which is what the
	// operator-facing counters say apart from "nothing happened".
	if mirrored2 != 250 {
		t.Fatalf("mirrored=%d over a 250-issue window: a pass that wrote nothing still covered it", mirrored2)
	}
}

// The manual "Sync now" endpoint is the third spender of the same issue
// windows — with the sweep's full page budget, on whichever replica served
// the request, under no lease. Its requests must carry their own lane (the
// hourly tally files an unlabelled request under "other", where an operator
// cannot tell this spend from any other interactive forge call), and its
// answer must report the window it MIRRORED: counting only the cards it had
// to write answers "0 synced" over a window that is re-read, which reads as
// a sync that did nothing.
//
// A window is re-read whenever the mark did not move — the pass after one an
// incomplete read refused, or an import given no `--since` over a board that
// already holds the cards. A click whose mark DID advance asks for the window
// since that mark, which is empty, and answers 0/0/0 under either meaning.
func TestHandleSyncForgeIntegration_ReportsTheWindowOnItsOwnLane(t *testing.T) {
	gh := newFakeGitHubIssues(t, 3, 0, 0)
	mark := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	board := newTestBoard(t)
	s, ri := seedSyncIntegration(t, gh.srv.URL, board, mark)
	s.logger = iterlog.New(iterlog.LevelInfo, &lockedBuffer{})

	syncNowOn := func(s *Server, ri forge.RepoIntegration) syncResult {
		t.Helper()
		r := forgeReq(superAdminCtx(), http.MethodPost,
			"/api/teams/t1/forge/integrations/"+ri.ID+"/sync", "", ri.TenantID)
		r.SetPathValue("iid", ri.ID)
		w := httptest.NewRecorder()
		s.handleSyncForgeIntegration(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("sync now: code=%d body=%s", w.Code, w.Body.String())
		}
		var got syncResult
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode %q: %v", w.Body.String(), err)
		}
		return got
	}

	first := syncNowOn(s, ri)
	// The same window again from a mark that did NOT move, over the board the
	// first pass filled: nothing to write, and that is the answer an operator
	// has to be able to read.
	sHeld, riHeld := seedSyncIntegration(t, gh.srv.URL, board, mark)
	sHeld.logger = iterlog.New(iterlog.LevelInfo, &lockedBuffer{})
	second := syncNowOn(sHeld, riHeld)
	lanes := laneTally(t, s)
	t.Logf("OBSERVED first=%+v second (held mark)=%+v lanes=%v", first, second, lanes)
	// And "0 synced" keeps its own meaning: a window with nothing in it.
	empty := newFakeGitHubIssues(t, 0, 0, 0)
	s2, ri2 := seedSyncIntegration(t, empty.srv.URL, newTestBoard(t), mark)
	s2.logger = iterlog.New(iterlog.LevelInfo, &lockedBuffer{})
	r2 := forgeReq(superAdminCtx(), http.MethodPost,
		"/api/teams/t1/forge/integrations/"+ri2.ID+"/sync", "", ri2.TenantID)
	r2.SetPathValue("iid", ri2.ID)
	w2 := httptest.NewRecorder()
	s2.handleSyncForgeIntegration(w2, r2)
	t.Logf("OBSERVED an empty window: code=%d body=%s", w2.Code, strings.TrimSpace(w2.Body.String()))
	if w2.Code != http.StatusOK || strings.TrimSpace(w2.Body.String()) != `{"synced":0,"created":0,"updated":0}` {
		t.Fatalf("an empty window answered code=%d %s, want 0/0/0 — that is what \"nothing to mirror\" says",
			w2.Code, w2.Body.String())
	}
	if first.Synced != 3 || first.Created != 3 {
		t.Fatalf("first click = %+v, want 3 synced / 3 created", first)
	}
	if second.Synced != 3 || second.Created != 0 || second.Updated != 0 {
		t.Fatalf("the click on a held mark = %+v: it mirrored 3 issues and wrote none — "+
			"reporting only the writes answers \"Synced 0 issues\" over a window that IS re-read", second)
	}
	if lanes[forgeLaneBoardManualSync] == 0 || lanes[""] > 0 {
		t.Fatalf("the manual sync's requests are charged to %v, not to %q — the hourly tally reports an unlabelled request as \"other\"",
			lanes, forgeLaneBoardManualSync)
	}
}

// syncNowBody clicks "Sync now" on one integration and answers the raw
// counters body, the three numbers an operator reads.
func syncNowBody(t *testing.T, s *Server, ri forge.RepoIntegration) string {
	t.Helper()
	r := forgeReq(superAdminCtx(), http.MethodPost,
		"/api/teams/t1/forge/integrations/"+ri.ID+"/sync", "", ri.TenantID)
	r.SetPathValue("iid", ri.ID)
	w := httptest.NewRecorder()
	s.handleSyncForgeIntegration(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("sync now: code=%d body=%s", w.Code, w.Body.String())
	}
	return strings.TrimSpace(w.Body.String())
}

// And what the operator actually sees clicking "Sync now" twice on a repo
// NOTHING touched in the last minute: the first click stamps the mark one
// overlap behind its read start, so the second asks the forge for the window
// since that stamp — empty here, because every entry is older than it — and
// answers 0/0/0. That is the healthy answer, under the mirrored meaning
// exactly as under the written-cards one. The sibling below is the other half:
// inside the overlap the same healthy repo answers its window again.
func TestHandleSyncForgeIntegration_ASecondClickReadsOnlyWhatMovedSince(t *testing.T) {
	mark := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	// Updated inside the first click's window and well before its stamp: the
	// forges take `since` at second resolution, so an entry touched in the
	// same second as the mark is served again — the safe direction, and not
	// what this witness is about.
	gh := newFakeGitHubIssues(t, 3, 0, 0, func(int) time.Time { return mark.Add(10 * time.Minute) })
	s, ri := seedSyncIntegration(t, gh.srv.URL, newTestBoard(t), mark)
	s.logger = iterlog.New(iterlog.LevelInfo, &lockedBuffer{})

	first, second := syncNowBody(t, s, ri), syncNowBody(t, s, ri)
	t.Logf("OBSERVED first=%s second=%s mark=%s (was %s)", first, second, storedMark(t, s, ri.ID), mark)
	if first != `{"synced":3,"created":3,"updated":0}` {
		t.Fatalf("first click = %s, want 3 synced / 3 created", first)
	}
	if second != `{"synced":0,"created":0,"updated":0}` {
		t.Fatalf("second click = %s, want 0/0/0 — the first click's mark narrows the window to what moved since, "+
			"so a repo nothing changed on answers an empty window, not its history", second)
	}
}

// The other half, and the one a LIVE repo answers: the mark is stamped one
// overlap behind the read's start, so a repo something touched inside that
// minute mirrors its window AGAIN on a second click — all mirrored, nothing
// written — with a mark that DID advance. So the non-empty re-read shape does
// not say the sync is stuck; the operator doc has to name both states that
// produce it, because the counters cannot tell them apart.
func TestHandleSyncForgeIntegration_ASecondClickInsideTheOverlapRereadsTheWindow(t *testing.T) {
	mark := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	// Touched seconds ago, i.e. inside the overlap the first click's mark
	// leaves behind: the steady state of any repo with activity.
	recent := time.Now().UTC().Add(-5 * time.Second)
	gh := newFakeGitHubIssues(t, 3, 0, 0, func(int) time.Time { return recent })
	s, ri := seedSyncIntegration(t, gh.srv.URL, newTestBoard(t), mark)
	s.logger = iterlog.New(iterlog.LevelInfo, &lockedBuffer{})

	first, second := syncNowBody(t, s, ri), syncNowBody(t, s, ri)
	advanced := storedMark(t, s, ri.ID)
	t.Logf("OBSERVED first=%s second=%s mark=%s (was %s) entries updated_at=%s overlap=%s",
		first, second, advanced, mark, recent, forgeIssueSyncMarkOverlap)
	if first != `{"synced":3,"created":3,"updated":0}` {
		t.Fatalf("first click = %s, want 3 synced / 3 created", first)
	}
	if second != `{"synced":3,"created":0,"updated":0}` {
		t.Fatalf("second click = %s, want 3/0/0 — the overlap re-reads the last %s of the window, so a repo "+
			"touched inside it mirrors its entries again and writes none", second, forgeIssueSyncMarkOverlap)
	}
	if !advanced.After(mark) {
		t.Fatalf("mark = %s, not past the seeded %s: this re-read must come from the overlap, not from a mark "+
			"that never moved — those are the two states the counters cannot tell apart", advanced, mark)
	}
}

// The refusal must price the window it could not finish in ENTRIES, never in
// the per-page size it ASKED for: an instance clamping to its own
// MAX_RESPONSE_ITEMS serves half of what multiplying the pages by that size
// suggests, so an operator sizing the page budget from that line sizes it
// wrong by the clamp factor — on exactly the provider the empty-page rule
// exists for.
func TestForEachForgeIssuePage_TheRefusalPricesTheWindowInEntries(t *testing.T) {
	ic := &clampingIssueClient{issues: testIssues(1000), limit: 50}
	ctx := withForgeIssuePageBudget(context.Background(), 3)

	err := forEachForgeIssuePage(ctx, ic, "org/api", time.Time{}, func([]forge.IssueRef) {})
	t.Logf("OBSERVED %v", err)
	if err == nil {
		t.Fatal("a window the budget could not exhaust must surface")
	}
	if !strings.Contains(err.Error(), "150 entries read in 3 pages") {
		t.Fatalf("error = %v, want the entries it did read named", err)
	}
	if strings.Contains(err.Error(), "pages of "+strconv.Itoa(forgeIssueSyncPerPage)) {
		t.Fatalf("error = %v: the line prices 3 pages at %d entries each over the 150 it read — "+
			"the size asked for is a request, not what the instance served", err, forgeIssueSyncPerPage)
	}
}

// An operator's "Sync now" carries the SWEEP's page budget, not a narrower
// one: the click is an explicit request for this repo's window, and a cap
// below the sweep's would make the manual path mirror less than the schedule
// already does. What it owes the operator is the refusal — 502, naming the
// budget that cut the window — rather than a success over a window it only
// partly read.
func TestHandleSyncForgeIntegration_CarriesTheSweepsPageBudget(t *testing.T) {
	// A window that never ends: every page is full, so only the budget stops
	// the read.
	gh := newFakeGitHubIssues(t, forgeIssueSyncMaxPages*forgeIssueSyncPerPage+1, 0, 0)
	mark := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	board := &countingBoard{}
	s, ri := seedSyncIntegration(t, gh.srv.URL, board, mark)
	s.logger = iterlog.New(iterlog.LevelInfo, &lockedBuffer{})

	r := forgeReq(superAdminCtx(), http.MethodPost,
		"/api/teams/t1/forge/integrations/"+ri.ID+"/sync", "", ri.TenantID)
	r.SetPathValue("iid", ri.ID)
	w := httptest.NewRecorder()
	s.handleSyncForgeIntegration(w, r)

	_, pages := gh.served()
	t.Logf("OBSERVED code=%d pages=%d mirrored-cards=%d body=%s", w.Code, pages, board.count(), strings.TrimSpace(w.Body.String()))
	if pages != forgeIssueSyncMaxPages {
		t.Fatalf("the click read %d pages, want the sweep's %d: a narrower cap mirrors less than the schedule already does",
			pages, forgeIssueSyncMaxPages)
	}
	if w.Code != http.StatusBadGateway ||
		!strings.Contains(w.Body.String(), strconv.Itoa(forgeIssueSyncMaxPages)+" pages") {
		t.Fatalf("a window the budget could not exhaust answered code=%d %s, want 502 naming the budget", w.Code, w.Body.String())
	}
	if got := storedMark(t, s, ri.ID); !got.Equal(mark) {
		t.Fatalf("the click moved the mark to %s over a window it only partly read", got)
	}
}
