package runview

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The run listings answer from a store's indexed window when it has one
// (store.RunWindowListingStore) and from the id-by-id scan otherwise. The two
// paths must answer the same thing — same rows, same order, same pages, same
// next tokens — for every filter combination. This file holds the shared
// fixture and the comparison; the filesystem row runs here, the Mongo row in
// run_listing_window_mongo_test.go.

// listingSeed is one run of the shared fixture.
type listingSeed struct {
	id, team, status, workflow, repo, bundle string
	created, updated                         time.Duration // offsets from listingBase
	format                                   store.ValueFormat
}

// listingBase is millisecond-aligned: Mongo stores dates to the millisecond.
var listingBase = time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)

// listingSeeds spreads runs over two teams and a tenant-less run, several
// statuses, workflows, repos and bundles, and ties THREE runs on one
// created_at instant so the id tie-break decides their order and a page
// boundary can fall inside the tie.
func listingSeeds() []listingSeed {
	s := time.Second
	return []listingSeed{
		{"r01", "team-a", "finished", "wf-x", "org/repo1", "bot-a", 0, 10 * s, 1},
		{"r02", "team-a", "failed", "wf-y", "", "", 1 * s, 1 * s, 2},
		{"r03", "team-b", "finished", "wf-x", "org/repo1", "Bot-A", 2 * s, 20 * s, 1},
		{"r04", "team-b", "running", "wf-x", "org/repo2", "", 2 * s, 5 * s, 2},
		{"r05", "team-a", "finished", "wf-y", "org/repo1", "bot-b", 2 * s, 30 * s, 1},
		{"r06", "", "finished", "wf-x", "", "", 3 * s, 3 * s, 2},
		{"r07", "team-a", "queued", "wf-x", "", "", 4 * s, 40 * s, 2},
		{"r08", "team-b", "finished", "wf-y", "org/repo2", "bot-a", 5 * s, 5 * s, 1},
		{"r09", "team-a", "finished", "wf-x", "org/repo2", "bot-a", 6 * s, 6 * s, 2},
		{"r10", "team-a", "finished", "wf-x", "org/repo1", "bot-a", 7 * s, 50 * s, 1},
		{"r11", "team-a", "finished", "wf-x", "org/repo1", "", 8 * s, 8 * s, 2},
		{"r12", "team-a", "failed", "wf-x", "org/repo1", "", 9 * s, 9 * s, 1},
	}
}

// Fixture roles beyond the plain runs: one deleted, and the undecodable ones
// each store can stage (a corrupt document never fails a page).
const (
	listingDeletedID = "r10"
	listingCorruptID = "r11"
)

// listingTeamCases are the per-team listing filters, each run at several
// limits. Repo, Bundle and Node are the filters only Go evaluates: with one
// set and a limit, a store that stopped at Limit rows would answer short.
func listingTeamCases() []ListFilter {
	since := listingBase.Add(5 * time.Second)
	var out []ListFilter
	for _, f := range []ListFilter{
		{},
		{Status: "finished"},
		{Workflow: "wf-x"},
		{Since: since},
		{Since: since.Add(500 * time.Microsecond)}, // sub-millisecond: Mongo rounds the stored side
		{Since: listingBase.Add(20 * time.Second)}, // equal to a run's updated_at: >= keeps it
		{Repo: "org/repo1"},                        // Go-only
		{Bundle: "BOT-A"},                          // Go-only, case-insensitive
		{Node: "listed-node"},                      // Go-only, reads events
		{Status: "finished", Workflow: "wf-x", Repo: "org/repo1"},
		{Workflow: "wf-y", Since: listingBase.Add(time.Second)},
		{Status: "failed"},
		{Status: "cancelled"},
	} {
		for _, limit := range []int{0, 1, 2, 3, 100} {
			f.Limit = limit
			out = append(out, f)
		}
	}
	return out
}

// listingAdminCases are the cross-tenant filters, each paged at several
// limits and from several hand-built cursors.
func listingAdminCases() []AdminListFilter {
	tie := listingBase.Add(2 * time.Second)
	var out []AdminListFilter
	for _, f := range []AdminListFilter{
		{},
		{Status: "finished"},
		{TeamIDs: []string{"team-a"}},
		{TeamIDs: []string{"team-b", "team-a"}},
		{TeamIDs: []string{""}}, // the tenant-less runs
		{TeamIDs: []string{"team-none"}},
		{Since: listingBase.Add(5 * time.Second)},
		{Since: listingBase.Add(5*time.Second + 500*time.Microsecond)},
		{Status: "finished", TeamIDs: []string{"team-b"}, Since: listingBase.Add(time.Second)},
	} {
		for _, limit := range []int{0, 1, 2, 3, 100} {
			f.Limit = limit
			out = append(out, f)
			for _, after := range []RunCursor{
				{CreatedAt: tie, ID: "r04"},                                // inside the tie
				{CreatedAt: tie, ID: "r00"},                                // below every id of the tie
				{CreatedAt: tie.Add(500 * time.Microsecond), ID: "r00"},    // between two storable instants
				{CreatedAt: listingBase.Add(9 * time.Second), ID: "zzzzz"}, // above every id of its instant
			} {
				c := f
				c.After = after
				out = append(out, c)
			}
		}
	}
	return out
}

// assertListingsAgree runs every case on the scan path (want) and the window
// path (got) and fails on the first difference. teamCtx scopes the per-team
// listing; the cross-tenant one lifts the scope itself. It returns how many
// rows the window path listed in total, so a caller can tell a fixture that
// listed nothing from one that agreed.
func assertListingsAgree(t *testing.T, want, got *Service, teamCtx context.Context) int {
	t.Helper()
	rows := 0
	for _, f := range listingTeamCases() {
		w, err := want.ListRunRecordsCtx(teamCtx, f)
		if err != nil {
			t.Fatalf("scan ListRunRecordsCtx(%+v): %v", f, err)
		}
		g, err := got.ListRunRecordsCtx(teamCtx, f)
		if err != nil {
			t.Fatalf("window ListRunRecordsCtx(%+v): %v", f, err)
		}
		if !slices.Equal(runIDs(w), runIDs(g)) {
			t.Errorf("ListRunRecordsCtx(%+v):\n scan   %v\n window %v", f, runIDs(w), runIDs(g))
		}
		for _, r := range g {
			if !r.SourceOmitted {
				t.Errorf("ListRunRecordsCtx(%+v): run %s carries its recorded source — a listing record is projected", f, r.ID)
			}
		}
		rows += len(g)
	}
	ctx := context.Background()
	for _, f := range listingAdminCases() {
		wantPages := adminPages(t, want, ctx, f)
		gotPages := adminPages(t, got, ctx, f)
		if !slices.EqualFunc(wantPages, gotPages, func(a, b adminPage) bool {
			return a.next == b.next && slices.Equal(a.ids, b.ids)
		}) {
			t.Errorf("ListAcrossTenants(%+v) pages:\n scan   %v\n window %v", f, wantPages, gotPages)
		}
		for _, p := range gotPages {
			rows += len(p.ids)
		}
	}
	return rows
}

type adminPage struct {
	ids  []string
	next string
}

func (p adminPage) String() string { return fmt.Sprintf("%v→%q", p.ids, p.next) }

// adminPages walks ListAcrossTenants from f.After until the answer names no
// next page.
func adminPages(t *testing.T, svc *Service, ctx context.Context, f AdminListFilter) []adminPage {
	t.Helper()
	var pages []adminPage
	for range 50 {
		page, next, err := svc.ListAcrossTenants(ctx, f)
		if err != nil {
			t.Fatalf("ListAcrossTenants(%+v): %v", f, err)
		}
		ids := make([]string, 0, len(page))
		for _, r := range page {
			ids = append(ids, r.ID)
		}
		pages = append(pages, adminPage{ids, next})
		if next == "" {
			return pages
		}
		if f.After, err = DecodeRunCursor(next); err != nil {
			t.Fatalf("DecodeRunCursor(%q): %v", next, err)
		}
	}
	t.Fatalf("ListAcrossTenants(%+v) never reached a last page", f)
	return nil
}

func runIDs(runs []*store.Run) []string {
	out := make([]string, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.ID)
	}
	return out
}

// fsWindowStore gives the filesystem store the indexed-window capability, as
// a double of the store contract: it answers a RunWindow exactly as the
// contract states (filters, newest-first order with the id tie-break, keyset,
// limit counted in records). It exists so the runview side — which filters
// go down, when the limit goes down, the next-page probe — is proven on every
// machine, Mongo or not.
type fsWindowStore struct {
	store.RunStore
	reads []store.RunWindow
}

func (s *fsWindowStore) Unwrap() store.RunStore { return s.RunStore }

func (s *fsWindowStore) ListRunWindow(ctx context.Context, w store.RunWindow) ([]*store.Run, []store.SkippedRun, error) {
	s.reads = append(s.reads, w)
	ids, err := s.ListRuns(ctx)
	if err != nil {
		return nil, nil, err
	}
	lister := store.AsRunListingStore(s.RunStore)
	var out []*store.Run
	var skipped []store.SkippedRun
	for _, id := range ids {
		r, err := lister.LoadRunForListing(ctx, id)
		if err != nil {
			skipped = append(skipped, store.SkippedRun{ID: id, Err: err})
			continue
		}
		if w.Status != "" && r.Status != w.Status ||
			w.Workflow != "" && r.WorkflowName != w.Workflow ||
			!w.Since.IsZero() && r.UpdatedAt.Before(w.Since) ||
			!teamAllowed(r.TenantID, w.TenantIDs) {
			continue
		}
		if !w.Before.CreatedAt.IsZero() && (r.CreatedAt.After(w.Before.CreatedAt) ||
			r.CreatedAt.Equal(w.Before.CreatedAt) && r.ID >= w.Before.ID) {
			continue
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b *store.Run) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(b.ID, a.ID)
	})
	if w.Limit > 0 && len(out) > w.Limit {
		out = out[:w.Limit]
	}
	return out, skipped, nil
}

// seedListingFS writes the fixture into a filesystem store: every run saved
// whole with its fields, the deleted one deleted, the corrupt one's run.json
// replaced by bytes no decoder reads.
func seedListingFS(t *testing.T, dir string, st store.RunStore) {
	t.Helper()
	ctx := context.Background()
	for _, s := range listingSeeds() {
		r := &store.Run{
			ID:           s.id,
			WorkflowName: s.workflow,
			Status:       store.RunStatus(s.status),
			TenantID:     s.team,
			ProjectPath:  s.repo,
			BundleName:   s.bundle,
			CreatedAt:    listingBase.Add(s.created),
			UpdatedAt:    listingBase.Add(s.updated),
		}
		if err := st.SaveRun(ctx, r); err != nil {
			t.Fatalf("seed %s: %v", s.id, err)
		}
	}
	seedListingNodeEvents(t, ctx, st)
	if err := st.DeleteRun(ctx, listingDeletedID); err != nil {
		t.Fatalf("delete %s: %v", listingDeletedID, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "runs", listingCorruptID, "run.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("corrupt %s: %v", listingCorruptID, err)
	}
}

// listingNodeRuns are the runs whose journal shows the node the Node filter
// asks for.
var listingNodeRuns = []string{"r01", "r05", "r12"} // legacy-format runs: an event append names its run's format

func seedListingNodeEvents(t *testing.T, ctx context.Context, st store.RunStore) {
	t.Helper()
	for _, id := range listingNodeRuns {
		if _, err := st.AppendEvent(ctx, id, store.Event{Type: store.EventNodeStarted, NodeID: "listed-node"}); err != nil {
			t.Fatalf("append node event to %s: %v", id, err)
		}
	}
}

// TestRunListingWindowAgreesWithTheScan_Filesystem is the filesystem row: the
// scan path (the filesystem store as it is) against the window path (the same
// store behind fsWindowStore), every filter, limit and cursor.
func TestRunListingWindowAgreesWithTheScan_Filesystem(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	seedListingFS(t, dir, st)

	scan, err := NewService(dir, WithStore(st), WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService(scan): %v", err)
	}
	windowed := &fsWindowStore{RunStore: st}
	window, err := NewService(dir, WithStore(windowed), WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService(window): %v", err)
	}
	if rows := assertListingsAgree(t, scan, window, context.Background()); rows == 0 {
		t.Fatal("the window path listed no row at all — the fixture is not exercised")
	}
	if len(windowed.reads) == 0 {
		t.Fatal("the window path never asked the store's window — the services compared the scan with itself")
	}
}

// The limit goes down to the store only when every filter is one the store
// evaluates; the cross-tenant view asks one row past it to learn whether a
// next page exists.
func TestRunListingWindowPushesTheLimitOnlyWhenTheStoreFiltersEverything(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	seedListingFS(t, dir, st)
	windowed := &fsWindowStore{RunStore: st}
	svc, err := NewService(dir, WithStore(windowed), WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	ctx := context.Background()
	for _, tc := range []struct {
		f         ListFilter
		wantLimit int
	}{
		{ListFilter{Limit: 2}, 2},
		{ListFilter{Status: "finished", Workflow: "wf-x", Since: listingBase, Limit: 2}, 2},
		{ListFilter{Repo: "org/repo1", Limit: 2}, 0},
		{ListFilter{Bundle: "bot-a", Limit: 2}, 0},
		{ListFilter{Node: "listed-node", Limit: 2}, 0},
	} {
		windowed.reads = nil
		if _, err := svc.ListRunRecordsCtx(ctx, tc.f); err != nil {
			t.Fatalf("ListRunRecordsCtx(%+v): %v", tc.f, err)
		}
		if len(windowed.reads) != 1 || windowed.reads[0].Limit != tc.wantLimit {
			t.Errorf("ListRunRecordsCtx(%+v) asked the store %+v, want one read with Limit %d", tc.f, windowed.reads, tc.wantLimit)
		}
	}
	windowed.reads = nil
	if _, _, err := svc.ListAcrossTenants(ctx, AdminListFilter{Limit: 3}); err != nil {
		t.Fatalf("ListAcrossTenants: %v", err)
	}
	if len(windowed.reads) != 1 || windowed.reads[0].Limit != 4 {
		t.Errorf("ListAcrossTenants(limit 3) asked the store %+v, want one read with Limit 4", windowed.reads)
	}
}
