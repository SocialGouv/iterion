package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/lease"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The incremental issue read and its high-water mark.
//
// The mark is the ONLY way back to an issue the sync did not see: the forge is
// queried with `since`, so an issue whose update falls inside a window the mark
// jumped over is never offered again. These probes pin the two halves of that:
// the read covers the whole window (every page), and the mark moves only over a
// read that completed.

// pagedIssueClient is a forge.IssueClient whose ListIssues answers one PAGE at
// a time, as the three providers do, and can fail a chosen page. It records the
// options of every call so a test reads what was actually asked for.
type pagedIssueClient struct {
	mu sync.Mutex
	// issues is the whole window, in provider order; ListIssues slices it by
	// the page it is asked for.
	issues []forge.IssueRef
	// failPage (1-based, 0 = never) is the page that answers an error, and
	// every later one.
	failPage int
	// fullPages, when > 0, makes every page a FULL one whatever the window
	// holds — a window no page budget can exhaust.
	fullPages bool
	// onCall, when set, runs before each page is served: a test reads the
	// board's state AS the read progresses.
	onCall func(page int)
	opts   []forge.IssueListOptions
}

func (p *pagedIssueClient) calls() []forge.IssueListOptions {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]forge.IssueListOptions(nil), p.opts...)
}

func (p *pagedIssueClient) ListIssues(_ context.Context, _ string, opts forge.IssueListOptions) ([]forge.IssueRef, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.opts = append(p.opts, opts)
	page := max(opts.Page, 1)
	if p.onCall != nil {
		p.onCall(page)
	}
	per := opts.PerPage
	if per <= 0 {
		per = 50
	}
	if p.failPage > 0 && page >= p.failPage {
		return nil, fmt.Errorf("list issues: page %d: 502 Bad Gateway", page)
	}
	if p.fullPages {
		out := make([]forge.IssueRef, 0, per)
		for i := range per {
			n := (page-1)*per + i + 1
			out = append(out, forge.IssueRef{Number: n, Title: "issue " + strconv.Itoa(n), State: "open"})
		}
		return out, nil
	}
	start := (page - 1) * per
	if start >= len(p.issues) {
		return nil, nil
	}
	return p.issues[start:min(start+per, len(p.issues))], nil
}

func (p *pagedIssueClient) GetIssue(context.Context, string, int) (forge.IssueRef, error) {
	return forge.IssueRef{}, errors.New("unused")
}

func (p *pagedIssueClient) CreateIssue(context.Context, string, forge.NewIssue) (forge.IssueRef, error) {
	return forge.IssueRef{}, errors.New("unused")
}

func (p *pagedIssueClient) UpdateIssue(context.Context, string, int, forge.IssuePatch) (forge.IssueRef, error) {
	return forge.IssueRef{}, errors.New("unused")
}

func (p *pagedIssueClient) CommentIssue(context.Context, string, int, string) (forge.CommentRef, error) {
	return forge.CommentRef{}, errors.New("unused")
}

// countingBoard is a board that keeps no state: it answers "no card yet" and
// counts the creates. It makes a window of tens of thousands of issues cost
// nothing but the loop, which is what the page-budget probe needs.
type countingBoard struct {
	native.BoardStore
	mu      sync.Mutex
	created int
}

func (c *countingBoard) Board() (*native.Board, error) { return native.DefaultBoard(), nil }

func (c *countingBoard) Get(string) (*native.Issue, error) { return nil, tracker.ErrNotFound }

func (c *countingBoard) Create(in native.Issue) (*native.Issue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.created++
	return &in, nil
}

func (c *countingBoard) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.created
}

// boardReadCounter is a real board that counts the per-card READS the upsert
// spends on top of the forge's pages — the half of a re-read's cost that is
// not a forge request, and the one the cloud board store answers with an
// uncached read per card.
type boardReadCounter struct {
	native.BoardStore
	reads atomic.Int64
}

func (b *boardReadCounter) Get(id string) (*native.Issue, error) {
	b.reads.Add(1)
	return b.BoardStore.Get(id)
}

// TestSyncForgeIssuesToBoard_AReReadCostsOneBoardReadPerEntry prices the
// window a HELD mark re-reads on every pass: its pages, and one board read per
// entry — the upsert reads each entry's card before deciding, so a window
// nothing changed still costs its own size in board reads even though it
// writes nothing. That is the cost both the operator doc and
// forgeIssueSyncPerPage state; "pages and nothing else" would understate a
// held mark by a per-card read every five minutes, per repo.
func TestSyncForgeIssuesToBoard_AReReadCostsOneBoardReadPerEntry(t *testing.T) {
	const entries = 7
	board := &boardReadCounter{BoardStore: newTestBoard(t)}
	pass := func() (int, int, int) {
		m, c, u, err := syncForgeIssuesToBoard(
			context.Background(), &pagedIssueClient{issues: testIssues(entries)},
			forge.ProviderGitHub, "conn1", "org/api", board, time.Time{}, nil)
		if err != nil {
			t.Fatalf("sync: %v", err)
		}
		return m, c, u
	}
	pass()
	board.reads.Store(0)
	m, c, u := pass()
	reads := board.reads.Load()
	t.Logf("OBSERVED re-read of %d entries: mirrored=%d created=%d updated=%d board reads=%d", entries, m, c, u, reads)
	if c != 0 || u != 0 {
		t.Fatalf("the re-read wrote created=%d updated=%d, want 0/0 — the write-side cost is what an unchanged card already saves", c, u)
	}
	if reads != entries {
		t.Fatalf("the re-read spent %d board reads on a %d-entry window, want one per entry — the cost paragraph prices it per entry",
			reads, entries)
	}
}

// refusingBoard refuses every Create — a board store outage, which leaves the
// window's issues unmirrored however well the forge answered. nth, when set,
// narrows the outage to the creates whose rank is in it: a board that refuses
// PART of a window is what makes the count of what it took load-bearing.
type refusingBoard struct {
	native.BoardStore
	err error
	mu  sync.Mutex
	nth map[int]bool
	n   int
}

func (f *refusingBoard) Create(in native.Issue) (*native.Issue, error) {
	f.mu.Lock()
	f.n++
	rank := f.n
	f.mu.Unlock()
	if f.nth == nil || f.nth[rank] {
		return nil, f.err
	}
	return f.BoardStore.Create(in)
}

// TestSyncForgeIssuesToBoard_APartialOutageCountsTheRefusedIssues: a board
// outage takes PART of a window. The first refusal is kept as the cause — the
// others are the same outage said again — but how MANY issues it took has to
// travel with it: `mirrored` is the operator-facing count and it silently
// drops by the refused ones, so a window that lost three cards and one that
// lost one answer with the same single card's error and a number that explains
// neither. The window then re-reads every five minutes (the mark holds) with
// nothing saying how much of it the board is missing.
func TestSyncForgeIssuesToBoard_APartialOutageCountsTheRefusedIssues(t *testing.T) {
	const entries = 5
	board := &refusingBoard{
		BoardStore: newTestBoard(t),
		err:        errors.New("boardmongo: create issue: i/o timeout"),
		nth:        map[int]bool{2: true, 3: true, 4: true},
	}
	mirrored, created, updated, err := syncForgeIssuesToBoard(
		context.Background(), &pagedIssueClient{issues: testIssues(entries)},
		forge.ProviderGitHub, "conn1", "org/api", board, time.Time{}, nil)
	t.Logf("OBSERVED mirrored=%d created=%d updated=%d err=%v", mirrored, created, updated, err)
	if err == nil {
		t.Fatal("a window the board partly refused was reported as mirrored whole")
	}
	if mirrored != 2 || created != 2 || updated != 0 {
		t.Fatalf("mirrored=%d created=%d updated=%d, want 2/2/0 — the entries the board took are the ones mirrored", mirrored, created, updated)
	}
	if !strings.Contains(err.Error(), "3 of 5 issues refused") {
		t.Errorf("error = %v, want the count of what the board refused: `synced` drops from 5 to 2 and nothing else says why", err)
	}
	if !strings.Contains(err.Error(), "i/o timeout") {
		t.Errorf("error = %v, want the board's own failure as the cause — the count is not a replacement for it", err)
	}
}

func testIssues(n int) []forge.IssueRef {
	out := make([]forge.IssueRef, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, forge.IssueRef{Number: i, Title: "issue " + strconv.Itoa(i), State: "open"})
	}
	return out
}

// TestSyncForgeIssuesToBoard_ReadsEveryPage: a window wider than one page is
// read to its end. A single list call mirrors the first page and drops the
// rest for good — the mark then moves past issues nothing will offer again.
func TestSyncForgeIssuesToBoard_ReadsEveryPage(t *testing.T) {
	board := newTestBoard(t)
	ic := &pagedIssueClient{issues: testIssues(250)}
	// Cards on the board when each page is asked for. A read that STREAMS has
	// already mirrored page 1 when it asks for page 2; one that buffers the
	// whole window into memory first has mirrored nothing until the end — which
	// is what the first sweep of a repo with no mark would hold in RAM.
	var onBoard []int
	ic.onCall = func(int) {
		all, _ := board.List(native.ListFilter{})
		onBoard = append(onBoard, len(all))
	}

	_, created, updated, err := syncForgeIssuesToBoard(
		context.Background(), ic, forge.ProviderGitHub, "conn1", "org/api", board, time.Time{}, nil)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if created != 250 || updated != 0 {
		t.Fatalf("created=%d updated=%d, want 250/0 — the window is 250 issues and every one of them has to be mirrored", created, updated)
	}
	all, err := board.List(native.ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 250 {
		t.Fatalf("board holds %d cards, want 250", len(all))
	}
	calls := ic.calls()
	// 100 + 100 + 50 + the empty page that ENDS the window: a short page is
	// the provider's own choice of size (a Forgejo instance clamps it), never
	// a statement that the window is exhausted.
	if len(calls) != 4 {
		t.Fatalf("ListIssues called %d times, want 4 (100+100+50+the empty page that ends it): a 250-issue window is three pages plus the forge's own end-of-window answer", len(calls))
	}
	for i, o := range calls {
		if o.Page != i+1 {
			t.Errorf("call %d asked for page %d, want %d — the pages must be walked in order", i, o.Page, i+1)
		}
		if o.PerPage != 100 {
			t.Errorf("call %d asked for %d per page, want 100", i, o.PerPage)
		}
		if o.State != "all" {
			t.Errorf("call %d asked for state %q, want \"all\"", i, o.State)
		}
	}
	if want := []int{0, 100, 200, 250}; !slices.Equal(onBoard, want) {
		t.Errorf("cards on the board as each page was served = %v, want %v — the pages must be mirrored as they arrive, not buffered", onBoard, want)
	}
}

// TestSyncForgeIssuesToBoard_PartialReadIsAnError: a page that fails leaves the
// window partly unread. What DID arrive is mirrored — those issues are valid —
// but the failure is reported, because it is the caller's only cue to hold its
// high-water mark.
func TestSyncForgeIssuesToBoard_PartialReadIsAnError(t *testing.T) {
	board := newTestBoard(t)
	ic := &pagedIssueClient{issues: testIssues(250), failPage: 2}

	_, created, _, err := syncForgeIssuesToBoard(
		context.Background(), ic, forge.ProviderGitHub, "conn1", "org/api", board, time.Time{}, nil)
	if err == nil {
		t.Fatal("a failed page must surface: silently reporting success is what moves the mark over issues nobody read")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error = %v, want the provider's own failure named", err)
	}
	if created != 100 {
		t.Errorf("created=%d, want 100 — the page that DID arrive is valid and worth mirroring", created)
	}
}

// TestSyncForgeIssuesToBoard_TruncatedWindowIsLoud: a window no page budget can
// exhaust is reported as the incomplete read it is. The providers answer
// newest-first, so a mark advanced over a truncated read drops that window's
// tail for good, and the next read would cut at the same place.
func TestSyncForgeIssuesToBoard_TruncatedWindowIsLoud(t *testing.T) {
	board := &countingBoard{}
	ic := &pagedIssueClient{fullPages: true}

	_, created, _, err := syncForgeIssuesToBoard(
		context.Background(), ic, forge.ProviderGitHub, "conn1", "org/api", board, time.Time{}, nil)
	if err == nil {
		t.Fatal("a window the page budget could not exhaust must surface — a silent truncation repeats on every sweep")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(forgeIssueSyncMaxPages)) {
		t.Errorf("error = %v, want the page budget named so an operator knows what to raise", err)
	}
	calls := ic.calls()
	if len(calls) != forgeIssueSyncMaxPages {
		t.Fatalf("ListIssues called %d times, want the %d-page budget — the read must stop, not walk forever", len(calls), forgeIssueSyncMaxPages)
	}
	want := forgeIssueSyncMaxPages * forgeIssueSyncPerPage
	if created != want || board.count() != want {
		t.Errorf("created=%d (board %d), want %d — what the budget DID read is mirrored", created, board.count(), want)
	}
}

// seedSyncIntegration wires a server whose issue sync reads the REAL GitHub
// client against gh, writes to board, and keeps its integration in a memory
// store. It returns the integration, whose LastSyncedAt is the mark under test.
func seedSyncIntegration(t *testing.T, ghURL string, board native.BoardStore, mark time.Time) (*Server, forge.RepoIntegration) {
	t.Helper()
	s := newForgeTestServer(t)
	conn := forge.Connection{
		ID: "c-sync", TenantID: "t1", Provider: forge.ProviderGitHub, Kind: forge.KindPAT,
		Status: forge.StatusActive, ForgeBaseURL: ghURL, Purpose: forge.PurposeRuntime, CreatedAt: time.Now(),
	}
	sealed, err := forge.SealPAT(s.sealer, conn.ID, "ghp-token")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	conn.SealedPayload = sealed
	if err := s.forgeConnections.Create(context.Background(), conn); err != nil {
		t.Fatalf("create connection: %v", err)
	}
	ri := forge.RepoIntegration{
		ID: "i-sync", TenantID: "t1", ConnectionID: conn.ID, RepoFullName: "org/api",
		SyncIssuesEnabled: true, LastSyncedAt: mark, CreatedAt: time.Now(),
	}
	if err := s.forgeIntegrations.Create(context.Background(), ri); err != nil {
		t.Fatalf("create integration: %v", err)
	}
	s.cfg.CloudBoardFor = func(string) native.BoardStore { return board }
	return s, ri
}

// storedMark reads back the integration's persisted high-water mark.
func storedMark(t *testing.T, s *Server, id string) time.Time {
	t.Helper()
	got, err := s.forgeIntegrations.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get integration: %v", err)
	}
	return got.LastSyncedAt
}

// forgeIssueWindow answers one issues-endpoint request the way a forge does:
// the 1-based numbers of a total-entry repo whose stamp — their updated_at —
// is at or after the `since` the request asks for, cut to the page it asks
// for. The page size comes from `per_page` (GitHub) or `limit`
// (Forgejo/Gitea), clamped server-side to maxPerPage when that is positive —
// an instance's own MAX_RESPONSE_ITEMS. It also returns the raw `since` the
// request carried, so a test can assert what window was ASKED for and not
// only what came back.
//
// Honouring `since` is what makes an issue fixture a forge: a high-water mark
// is only ever spent as that parameter, so a fake serving the same rows for
// every window cannot tell an incremental read from a full-history one — and
// the pages, the stamping and the refusing all keep working either way.
//
// The cut has SECOND resolution, as on the forges: every client formats Since
// as RFC3339, so an entry updated in the same second as the mark is served
// again. Re-reading is the safe direction, and a fixture stamping its entries
// at its own birth cannot witness a window the mark narrowed.
func forgeIssueWindow(r *http.Request, total, maxPerPage int, stamp func(i int) time.Time) ([]int, string, error) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	per, _ := strconv.Atoi(q.Get("per_page"))
	if per <= 0 {
		per, _ = strconv.Atoi(q.Get("limit"))
	}
	page = max(page, 1)
	if maxPerPage > 0 && (per <= 0 || per > maxPerPage) {
		per = maxPerPage
	}
	per = max(per, 1)
	since := q.Get("since")
	var cut time.Time
	if since != "" {
		var err error
		if cut, err = time.Parse(time.RFC3339, since); err != nil {
			return nil, since, fmt.Errorf("since %q is not RFC3339: %w", since, err)
		}
	}
	win := make([]int, 0, total)
	for i := 1; i <= total; i++ {
		if cut.IsZero() || !stamp(i).Before(cut) {
			win = append(win, i)
		}
	}
	start := (page - 1) * per
	if start >= len(win) {
		return nil, since, nil
	}
	return win[start:min(start+per, len(win))], since, nil
}

// forgeIssueEntry is one GitHub-shaped row of the issues endpoint for entry i,
// carrying the updated_at forgeIssueWindow filters on.
func forgeIssueEntry(i int, stamp func(i int) time.Time) map[string]any {
	return map[string]any{
		"number": i, "title": "issue " + strconv.Itoa(i), "state": "open",
		"html_url":   "https://forge.example/org/api/issues/" + strconv.Itoa(i),
		"updated_at": stamp(i).UTC().Format(time.RFC3339),
	}
}

// fakeGitHubIssues serves GET /api/v3/repos/{repo}/issues the way GitHub does:
// one page at a time, `since`/`page`/`per_page` honoured. servedAt records when
// the FIRST page was served, and failFrom (1-based, 0 = never) fails that page
// and every later one. Entries are stamped at the fixture's birth unless the
// caller passes its own stamp, so a mark older than the fixture holds the whole
// repo and a test only has to care about the window when that is its subject.
type fakeGitHubIssues struct {
	mu       sync.Mutex
	failFrom int
	servedAt time.Time
	pages    int
	asked    []string
	srv      *httptest.Server
}

func newFakeGitHubIssues(t *testing.T, total, failFrom int, delay time.Duration, stamp ...func(i int) time.Time) *fakeGitHubIssues {
	t.Helper()
	if len(stamp) > 1 {
		t.Fatalf("newFakeGitHubIssues: %d stamps given, want at most one", len(stamp))
	}
	born := time.Now().UTC()
	at := func(int) time.Time { return born }
	if len(stamp) == 1 && stamp[0] != nil {
		at = stamp[0]
	}
	f := &fakeGitHubIssues{failFrom: failFrom}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		nums, since, err := forgeIssueWindow(r, total, 0, at)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		f.mu.Lock()
		f.pages++
		f.asked = append(f.asked, since)
		if f.servedAt.IsZero() {
			f.servedAt = time.Now().UTC()
		}
		fail := f.failFrom > 0 && max(page, 1) >= f.failFrom
		f.mu.Unlock()
		// The delay opens the window a mark stamped at the END of the sync
		// would declare synced without having read it.
		time.Sleep(delay)
		if fail {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"message":"server error"}`))
			return
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

func (f *fakeGitHubIssues) served() (time.Time, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.servedAt, f.pages
}

// windowsAsked returns the raw `since` of every page request, in order.
func (f *fakeGitHubIssues) windowsAsked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

// TestSyncOneIntegration_FailedListHoldsTheMark: the list call fails, so the
// sweep read nothing — and the mark must not move. Moving it writes off every
// issue updated inside the window it skipped: the forge is only ever asked for
// what changed SINCE the mark, so nothing offers them again.
func TestSyncOneIntegration_FailedListHoldsTheMark(t *testing.T) {
	gh := newFakeGitHubIssues(t, 10, 1, 0)
	mark := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	s, ri := seedSyncIntegration(t, gh.srv.URL, newTestBoard(t), mark)

	if _, _, _, err := s.syncOneIntegration(context.Background(), "t1", ri); err == nil {
		t.Fatal("a failed list must surface")
	}
	if got := storedMark(t, s, ri.ID); !got.Equal(mark) {
		t.Fatalf("mark moved to %s over a read that failed (was %s) — every issue updated in that window is now unreachable", got, mark)
	}
}

// TestSyncOneIntegration_PartialReadHoldsTheMark: page 1 arrived, page 2 did
// not. The cards of page 1 land, and the mark stays: the rest of the window is
// still owed, and only the mark can ask for it again.
func TestSyncOneIntegration_PartialReadHoldsTheMark(t *testing.T) {
	gh := newFakeGitHubIssues(t, 250, 2, 0)
	mark := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	board := newTestBoard(t)
	s, ri := seedSyncIntegration(t, gh.srv.URL, board, mark)

	_, created, _, err := s.syncOneIntegration(context.Background(), "t1", ri)
	if err == nil {
		t.Fatal("a half-read window must surface as a failure")
	}
	if created != 100 {
		t.Errorf("created=%d, want 100 — the page that arrived is mirrored", created)
	}
	if got := storedMark(t, s, ri.ID); !got.Equal(mark) {
		t.Fatalf("mark moved to %s over a half-read window (was %s) — pages 2 and 3 are lost", got, mark)
	}
}

// TestSyncOneIntegration_FailedUpsertHoldsTheMark: the read was complete but
// the board refused the writes. The issues are not on the board, so the window
// is still owed — the mark holds and the next sweep retries it (the upsert is
// idempotent, so a retry costs nothing but the pass).
func TestSyncOneIntegration_FailedUpsertHoldsTheMark(t *testing.T) {
	gh := newFakeGitHubIssues(t, 3, 0, 0)
	mark := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	board := &refusingBoard{BoardStore: newTestBoard(t), err: errors.New("boardmongo: create issue: i/o timeout")}
	s, ri := seedSyncIntegration(t, gh.srv.URL, board, mark)

	_, _, _, err := s.syncOneIntegration(context.Background(), "t1", ri)
	if err == nil {
		t.Fatal("a refused board write must surface")
	}
	if got := storedMark(t, s, ri.ID); !got.Equal(mark) {
		t.Fatalf("mark moved to %s over issues the board refused (was %s) — they are on no board and nothing will offer them again", got, mark)
	}
	// And the refusal SAYS the mark held: this wrapper is the only caller that
	// keeps one (the shared core also serves `iterion issue import`, which
	// keeps none), so the sentence belongs to it. Without it the operator
	// reads a refusal that could equally have written the window off.
	if !strings.Contains(err.Error(), "the high-water mark stays put") {
		t.Errorf("error = %v, want the held mark named — a refusal that does not say the window is still owed reads like a loss", err)
	}
}

// TestSyncOneIntegration_ReadsOnlyTheWindowSinceTheMark: the mark is spent
// exactly one way — as the `since` every page of the read asks the forge for.
// A read that drops it mirrors the repo's WHOLE history on every pass, paying
// the page budget of a 20 000-entry window per repo per interval against the
// installation's hourly REST allowance, while the stamping, the holding and
// the refusing all keep working: nothing but the window itself witnesses it.
func TestSyncOneIntegration_ReadsOnlyTheWindowSinceTheMark(t *testing.T) {
	mark := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	// Entries 1-3 were updated inside the window; 4-8 long before the mark,
	// so a pass that reads them read the repo's history, not its window.
	// Entry 9 was updated at the start of the mark's own SECOND, which is the
	// resolution `since` travels at (every client formats it RFC3339): it is
	// re-read, the direction that costs a page and not a lost issue.
	gh := newFakeGitHubIssues(t, 9, 0, 0, func(i int) time.Time {
		switch {
		case i <= 3:
			return mark.Add(10 * time.Minute)
		case i == 9:
			return mark.Truncate(time.Second)
		default:
			return mark.Add(-3 * time.Hour)
		}
	})
	board := newTestBoard(t)
	s, ri := seedSyncIntegration(t, gh.srv.URL, board, mark)

	mirrored, created, _, err := s.syncOneIntegration(context.Background(), "t1", ri)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	cards, err := board.List(native.ListFilter{})
	if err != nil {
		t.Fatalf("list board: %v", err)
	}
	asked := gh.windowsAsked()
	t.Logf("OBSERVED mirrored=%d created=%d cards=%d since asked=%q", mirrored, created, len(cards), asked)
	if len(asked) == 0 {
		t.Fatal("the fake forge was never asked for a page")
	}
	for i, since := range asked {
		if since == "" {
			t.Fatalf("page %d asked the forge for the repo's WHOLE history (no `since` in %q) — "+
				"the mark is then stamped, held and refused for nothing, and every pass re-reads everything the repo ever had",
				i+1, asked)
		}
	}
	if mirrored != 4 || created != 4 || len(cards) != 4 {
		t.Fatalf("mirrored=%d created=%d cards=%d, want 4 — the window is what changed SINCE the mark (entries 1-3) plus the entry "+
			"inside the mark's own second (9), never the repo's history: a mark carried at sub-second precision would skip that entry, "+
			"and nothing would ever offer it again",
			mirrored, created, len(cards))
	}
}

// TestSyncOneIntegration_CompleteReadMarksTheStart: a complete sweep advances
// the mark to one forgeIssueSyncMarkOverlap BEFORE the instant the read
// STARTED, never to the instant it ended. An issue updated while the sweep
// runs is then re-read by the next one; a mark stamped at the end declares it
// synced without ever having seen it. The overlap is the margin the forge's
// own clock and list index need (see the constant); it shifts both bounds of
// the window the mark may land in, and nothing else.
func TestSyncOneIntegration_CompleteReadMarksTheStart(t *testing.T) {
	gh := newFakeGitHubIssues(t, 3, 0, 150*time.Millisecond)
	mark := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	s, ri := seedSyncIntegration(t, gh.srv.URL, newTestBoard(t), mark)

	before := time.Now().UTC()
	_, created, _, err := s.syncOneIntegration(context.Background(), "t1", ri)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if created != 3 {
		t.Fatalf("created=%d, want 3", created)
	}
	servedAt, _ := gh.served()
	if servedAt.IsZero() {
		t.Fatal("the fake forge was never asked for a page")
	}
	got := storedMark(t, s, ri.ID)
	if !got.After(mark) {
		t.Fatalf("mark = %s, want it advanced past %s after a complete read", got, mark)
	}
	if lower := before.Add(-forgeIssueSyncMarkOverlap); got.Before(lower) {
		t.Fatalf("mark = %s, earlier than the sync's own start less the %s overlap (%s): the overlap is a bounded margin, not an unbounded rewind",
			got, forgeIssueSyncMarkOverlap, lower)
	}
	if upper := servedAt.Add(-forgeIssueSyncMarkOverlap); got.After(upper) {
		t.Fatalf("mark = %s, later than the instant the forge answered less the %s overlap (%s): an issue updated during the read, or one the forge "+
			"dated by its own clock, is declared synced without having been read",
			got, forgeIssueSyncMarkOverlap, upper)
	}
}

// forgeWithALaggingView serves the issues endpoint through the shared window
// helper, with two dials no other fixture here has: how many entries its list
// SHOWS right now, and the updated_at it reports for one chosen entry. They
// are the two ways a forge's answer disagrees with this pod's clock — a clock
// running behind, and a list index that has not caught up with its own writes
// — and both are read on every request, so a test can move them between
// passes the way the forge does between sweeps.
type forgeWithALaggingView struct {
	visible atomic.Int32 // entries the list serves right now
	late    atomic.Int64 // updated_at of entry lateEntry, unix nanos
	// lateEntry is the entry `late` dates; every other entry is dated settled.
	lateEntry int
	settled   time.Time
	srv       *httptest.Server
}

func newForgeWithALaggingView(t *testing.T, visible, lateEntry int, settled time.Time, late time.Time) *forgeWithALaggingView {
	t.Helper()
	f := &forgeWithALaggingView{lateEntry: lateEntry, settled: settled}
	f.visible.Store(int32(visible))
	f.late.Store(late.UnixNano())
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		nums, _, err := forgeIssueWindow(r, int(f.visible.Load()), 0, f.at)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		out := []map[string]any{}
		for _, i := range nums {
			out = append(out, forgeIssueEntry(i, f.at))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// at is the updated_at the forge reports for entry i — its OWN clock, which is
// what `since` is evaluated against.
func (f *forgeWithALaggingView) at(i int) time.Time {
	if i == f.lateEntry {
		return time.Unix(0, f.late.Load()).UTC()
	}
	return f.settled
}

// forgeClockLagInProbes is how far behind this pod the fixture's clock runs:
// bounded and physical, smaller than the overlap that has to cover it.
const forgeClockLagInProbes = 10 * time.Second

// TestSyncOneIntegration_TheMarkCoversAForgeClockBehindThisPod: the mark is
// stamped from THIS pod's clock but spent as the forge's `since`, which the
// forge evaluates against the updated_at ITS clock wrote. An entry the forge
// dates a few seconds before the mark — because its clock runs behind — is
// never offered again, and `since` is the only way back to it.
func TestSyncOneIntegration_TheMarkCoversAForgeClockBehindThisPod(t *testing.T) {
	mark := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	gh := newForgeWithALaggingView(t, 3, 4, mark.Add(10*time.Minute), mark.Add(10*time.Minute))
	board := newTestBoard(t)
	s, ri := seedSyncIntegration(t, gh.srv.URL, board, mark)

	pass1Start := time.Now().UTC()
	if _, created, _, err := s.syncOneIntegration(context.Background(), "t1", ri); err != nil {
		t.Fatalf("pass 1: %v", err)
	} else if created != 3 {
		t.Fatalf("pass 1 created=%d, want the 3 entries the list showed", created)
	}
	mark1 := storedMark(t, s, ri.ID)

	// Entry 4 is updated one second AFTER pass 1 started, so no window ending
	// at pass 1's start can honestly claim it — and the forge, its clock
	// behind, dates that update before the mark pass 1 wrote.
	gh.late.Store(pass1Start.Add(time.Second - forgeClockLagInProbes).UnixNano())
	gh.visible.Store(4)

	// Pass 2 as the sweep runs it: the integration is re-read, so the window
	// starts at the mark pass 1 stamped.
	fresh, err := s.forgeIntegrations.Get(context.Background(), ri.ID)
	if err != nil {
		t.Fatalf("re-read integration: %v", err)
	}
	if _, _, _, err := s.syncOneIntegration(context.Background(), "t1", fresh); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	cards, err := board.List(native.ListFilter{})
	if err != nil {
		t.Fatalf("list board: %v", err)
	}
	if !slices.ContainsFunc(cards, func(c *native.Issue) bool { return c != nil && c.External != nil && c.External.Number == 4 }) {
		t.Fatalf("the board holds %d cards and not the entry the forge dated %s before the mark: a mark stamped from this pod's clock "+
			"writes off every update a forge running behind dated inside that gap, and no pass ever reports it missing",
			len(cards), forgeClockLagInProbes)
	}
	// The overlap is a constant SHIFT, so across SERIAL passes — what this
	// witness runs, and what the sweep does per repo — the mark still moves
	// forward: an overlap applied to the previous MARK instead of the read's
	// start would walk it backwards one overlap per pass, widening its own
	// window for ever instead of following the repo. Passes in flight at the
	// same time are a different question, answered by the stamp's own contract
	// (pkg/forge: it arbitrates nothing).
	if mark2 := storedMark(t, s, ri.ID); !mark2.After(mark1) {
		t.Fatalf("pass 2 left the mark at %s, not after pass 1's %s — the overlap must shift the mark, never let it step back", mark2, mark1)
	}
}

// TestSyncOneIntegration_TheMarkCoversAListThatServedAnEntryLate is the same
// hazard with no clock disagreement at all: the entry's updated_at was already
// in the past when the pass read the list, and the list served it only
// afterwards. A mark at the read's own start writes it off for good.
func TestSyncOneIntegration_TheMarkCoversAListThatServedAnEntryLate(t *testing.T) {
	mark := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	gh := newForgeWithALaggingView(t, 1, 2, mark.Add(time.Minute), mark.Add(time.Minute))
	board := newTestBoard(t)
	s, ri := seedSyncIntegration(t, gh.srv.URL, board, mark)

	// Entry 2 was updated two seconds before the pass read the list — inside
	// the window it asked for — and the list index had not caught up.
	gh.late.Store(time.Now().UTC().Add(-2 * time.Second).UnixNano())
	if _, _, _, err := s.syncOneIntegration(context.Background(), "t1", ri); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	gh.visible.Store(2)

	fresh, err := s.forgeIntegrations.Get(context.Background(), ri.ID)
	if err != nil {
		t.Fatalf("re-read integration: %v", err)
	}
	if _, _, _, err := s.syncOneIntegration(context.Background(), "t1", fresh); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	cards, err := board.List(native.ListFilter{})
	if err != nil {
		t.Fatalf("list board: %v", err)
	}
	if len(cards) != 2 {
		t.Fatalf("the board holds %d card(s), want 2: the entry the forge's list did not serve during pass 1 is dated before the mark "+
			"pass 1 stamped, so no later window contains it", len(cards))
	}
}

// countingIntegrationStore counts the sweeps that reached it: the sweep's
// first act is to list the sync-enabled integrations, so one list is one pass.
type countingIntegrationStore struct {
	*forge.MemoryRepoIntegrationStore
	n atomic.Int64
}

func (c *countingIntegrationStore) ListSyncEnabled(ctx context.Context) ([]forge.RepoIntegration, error) {
	c.n.Add(1)
	return c.MemoryRepoIntegrationStore.ListSyncEnabled(ctx)
}

// TestBoardIssueSync_OnlyTheLeaseHolderSweeps is the fleet shape: several
// replicas start the issue sweep on one shared lease store — through
// runBoardSyncWorker, the very entry point ListenAndServe starts — and a
// replica sweeps only inside a term it was elected to.
//
// Every replica sweeping is the cost the lease removes: each pass lists EVERY
// sync-enabled repo's issue window on the forge, and the installation pays
// that once per replica. The invariant is read from each replica's own
// "elected" lines, not from a count of sweepers — the store's contract, not a
// clock, is what makes a term exclusive, and that is pinned by the conformance
// suite in pkg/lease.
func TestBoardIssueSync_OnlyTheLeaseHolderSweeps(t *testing.T) {
	shared := lease.NewMemoryStore()
	type replica struct {
		s    *Server
		log  *lockedBuffer
		ints *countingIntegrationStore
		stop context.CancelFunc
		done chan struct{}
	}
	const replicas = 3
	rs := make([]*replica, replicas)
	for i := range rs {
		log := &lockedBuffer{}
		ints := &countingIntegrationStore{MemoryRepoIntegrationStore: forge.NewMemoryRepoIntegrationStore()}
		s := &Server{
			leases:            shared,
			replicaID:         fmt.Sprintf("replica-%d", i),
			logger:            iterlog.New(iterlog.LevelInfo, log),
			forgeIntegrations: ints,
		}
		ctx, cancel := context.WithCancel(context.Background())
		r := &replica{s: s, log: log, ints: ints, stop: cancel, done: make(chan struct{})}
		rs[i] = r
		go func() {
			defer close(r.done)
			r.s.runBoardSyncWorker(ctx, 200*time.Millisecond)
		}()
	}
	t.Cleanup(func() {
		for _, r := range rs {
			r.stop()
			<-r.done
		}
	})

	total := func() (n int64) {
		for _, r := range rs {
			n += r.ints.n.Load()
		}
		return n
	}
	elected := func(r *replica) int {
		return strings.Count(r.log.String(), fmt.Sprintf("lease %q: %s elected", leaseBoardIssueSync, r.s.replicaID))
	}

	waitForCond(t, 10*time.Second, "several sweep passes", func() bool { return total() >= 5 })
	var unelected []string
	for _, r := range rs {
		if r.ints.n.Load() > 0 && elected(r) == 0 {
			unelected = append(unelected, r.s.replicaID)
		}
	}
	if len(unelected) > 0 {
		t.Fatalf("%v swept without ever being elected — every replica sweeping multiplies the forge's issue-list cost by the replica count", unelected)
	}
}

// TestBoardSyncPasses_ATermOpensWithAPass: the loop sweeps once on entry, then
// on every tick. A term that ends before its first tick — a lease-store blip,
// a rollout — would otherwise leave the elected net never actually sweeping,
// which is strictly worse than the unelected loop it replaces.
func TestBoardSyncPasses_ATermOpensWithAPass(t *testing.T) {
	ints := &countingIntegrationStore{MemoryRepoIntegrationStore: forge.NewMemoryRepoIntegrationStore()}
	s := &Server{forgeIntegrations: ints}

	// An interval far longer than the term: only an opening pass can land.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.boardSyncPasses(ctx, time.Hour)
	}()
	waitForCond(t, 5*time.Second, "the term's opening pass", func() bool { return ints.n.Load() >= 1 })
	cancel()
	<-done
	if n := ints.n.Load(); n != 1 {
		t.Fatalf("passes = %d in a term shorter than one interval, want exactly 1 (the opening pass)", n)
	}
}

// TestBoardIssueSync_AnUnusableLeaseRefusesLoudly: with no lease store the net
// cannot elect anybody, so it must not sweep — and must SAY so. A silent skip
// is the invisible outage: every repo's board would quietly stop converging
// with nothing anywhere naming the sweep that never ran.
func TestBoardIssueSync_AnUnusableLeaseRefusesLoudly(t *testing.T) {
	log := &lockedBuffer{}
	ints := &countingIntegrationStore{MemoryRepoIntegrationStore: forge.NewMemoryRepoIntegrationStore()}
	s := &Server{
		leases:            nil, // what a wiring regression would leave behind
		replicaID:         "replica-0",
		logger:            iterlog.New(iterlog.LevelInfo, log),
		forgeIntegrations: ints,
	}

	s.runBoardSyncWorker(context.Background(), time.Hour) // returns at once
	if n := ints.n.Load(); n != 0 {
		t.Errorf("passes = %d with no lease store, want 0 — an unelected sweep is what the lease exists to refuse", n)
	}
	if out := log.String(); !strings.Contains(out, "forge→board issue sync NOT started") {
		t.Fatalf("log = %q, want the refusal named: a net that cannot start and says nothing is an invisible outage", out)
	}
}
