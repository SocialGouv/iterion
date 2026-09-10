package server

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

var testProjectRef = forge.ProjectRef{Owner: "SocialGouv", Number: 203}

// testProject is the field schema of the real board this epic targets, with
// the ids it actually carries.
func testProject() forge.Project {
	return forge.Project{
		ID: "PVT_p", Number: 203, Title: "Iterion",
		Fields: []forge.ProjectField{
			{ID: "PVTSSF_status", Name: "Status", DataType: "SINGLE_SELECT", Options: []forge.ProjectFieldOption{
				{ID: "fb92b7a2", Name: "Inbox"},
				{ID: "6b7641c9", Name: "Planned"},
				{ID: "d360bd91", Name: "In progress"},
				{ID: "6b20abeb", Name: "Blocked"},
				{ID: "27139072", Name: "Done"},
			}},
			{ID: "PVTSSF_area", Name: "Area", DataType: "SINGLE_SELECT", Options: []forge.ProjectFieldOption{
				{ID: "8377b935", Name: "engine"}, {ID: "568d6b97", Name: "cloud/ops"},
			}},
			{ID: "PVTSSF_mode", Name: "Mode", DataType: "SINGLE_SELECT", Options: []forge.ProjectFieldOption{
				{ID: "c9116deb", Name: "dogfood"}, {ID: "15864718", Name: "direct"},
			}},
			{ID: "PVTSSF_prio", Name: "Priority", DataType: "SINGLE_SELECT", Options: []forge.ProjectFieldOption{
				{ID: "ebacc6b0", Name: "P0"}, {ID: "f9253403", Name: "P1"}, {ID: "b4c36e57", Name: "P2"},
			}},
		},
	}
}

// fakeBoardClient is an offline forge.BoardClient over in-memory pages.
type fakeBoardClient struct {
	project forge.Project
	pages   [][]forge.ProjectItem
	writes  []boardWrite
	err     error
	setErr  error
}

type boardWrite struct{ ProjectID, ItemID, FieldID, OptionID string }

func (f *fakeBoardClient) GetProject(context.Context, forge.ProjectRef) (forge.Project, error) {
	if f.err != nil {
		return forge.Project{}, f.err
	}
	return f.project, nil
}

func (f *fakeBoardClient) ListProjectItems(_ context.Context, _ forge.ProjectRef, opts forge.ProjectItemListOptions) (forge.ProjectItemPage, error) {
	if f.err != nil {
		return forge.ProjectItemPage{}, f.err
	}
	idx := 0
	if opts.Cursor != "" {
		n, err := parseCursor(opts.Cursor)
		if err != nil {
			return forge.ProjectItemPage{}, err
		}
		idx = n
	}
	if idx >= len(f.pages) {
		return forge.ProjectItemPage{}, nil
	}
	page := forge.ProjectItemPage{Items: f.pages[idx]}
	if idx+1 < len(f.pages) {
		page.HasNext = true
		page.NextCursor = "page-" + string(rune('0'+idx+1))
	}
	return page, nil
}

func parseCursor(c string) (int, error) {
	if !strings.HasPrefix(c, "page-") {
		return 0, errors.New("bad cursor")
	}
	return int(c[len("page-")] - '0'), nil
}

// ItemForIssue answers from the same pages the board serves, so the fake
// cannot claim an item the board does not carry.
func (f *fakeBoardClient) ItemForIssue(_ context.Context, _ forge.ProjectRef, repo string, number int) (forge.ProjectItem, bool, error) {
	if f.err != nil {
		return forge.ProjectItem{}, false, f.err
	}
	for _, page := range f.pages {
		for _, it := range page {
			if strings.EqualFold(it.Content.Repo, repo) && it.Content.Number == number {
				return it, true, nil
			}
		}
	}
	return forge.ProjectItem{}, false, nil
}

func (f *fakeBoardClient) IssueContentID(context.Context, string, int) (string, error) {
	return "", errors.New("not used")
}
func (f *fakeBoardClient) AddItem(context.Context, string, string) (forge.ProjectItem, error) {
	return forge.ProjectItem{}, errors.New("not used")
}
func (f *fakeBoardClient) SetSingleSelect(_ context.Context, projectID, itemID, fieldID, optionID string) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.writes = append(f.writes, boardWrite{projectID, itemID, fieldID, optionID})
	return nil
}

// item builds a project item backed by an iterion issue.
func item(id string, number int, fields ...forge.ProjectItemField) forge.ProjectItem {
	return forge.ProjectItem{
		ID: id,
		Content: forge.ProjectItemContent{
			Kind: forge.ProjectContentIssue, Repo: "SocialGouv/iterion",
			Number: number, Title: "t", State: "open",
		},
		Fields: fields,
	}
}

func statusValue(name string, at time.Time) forge.ProjectItemField {
	return forge.ProjectItemField{FieldID: "PVTSSF_status", FieldName: "Status", Value: name, UpdatedAt: at}
}

func selectValue(fieldID, name, value string) forge.ProjectItemField {
	return forge.ProjectItemField{FieldID: fieldID, FieldName: name, Value: value}
}

// seedCard creates a native card the way the issue import would.
func seedCard(t *testing.T, board native.BoardStore, number int, state string, labels ...string) string {
	t.Helper()
	id := forgeCardID(forge.ProviderGitHub, "SocialGouv/iterion", number)
	if _, err := board.Create(native.Issue{
		ID: id, Title: "t", State: state, Labels: labels,
		External: &native.ExternalRef{Provider: "github", Repo: "SocialGouv/iterion", Number: number},
	}); err != nil {
		t.Fatalf("seed card: %v", err)
	}
	return id
}

func mustGet(t *testing.T, board native.BoardStore, id string) *native.Issue {
	t.Helper()
	iss, err := board.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return iss
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

func TestImportProjectBoardAppliesStatusAndLabels(t *testing.T) {
	board := newTestBoard(t)
	id := seedCard(t, board, 613, native.StateInbox, "kind:epic")
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613,
			statusValue("In progress", time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)),
			selectValue("PVTSSF_area", "Area", "cloud/ops"),
			selectValue("PVTSSF_prio", "Priority", "P1"),
		),
	}}}

	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, nil)
	if err != nil {
		t.Fatalf("ImportProjectBoard: %v", err)
	}
	if res.Moved != 1 {
		t.Errorf("Moved = %d, want 1 (%+v)", res.Moved, res)
	}

	iss := mustGet(t, board, id)
	if iss.State != native.StateInProgress {
		t.Errorf("state = %q, want %q", iss.State, native.StateInProgress)
	}
	for _, want := range []string{"area:cloud-ops", "prio:p1", "kind:epic"} {
		if !slices.Contains(iss.Labels, want) {
			t.Errorf("labels %v missing %q", iss.Labels, want)
		}
	}
	// The sync state must be recorded, with the BOARD's own timestamp.
	if iss.External == nil || iss.External.Project == nil {
		t.Fatal("ExternalRef.Project not recorded")
	}
	p := iss.External.Project
	if p.Status != "In progress" || p.ItemID != "PVTI_1" || p.Number != 203 || p.Owner != "SocialGouv" {
		t.Errorf("project sync state wrong: %+v", p)
	}
	if !p.StatusAt.Equal(time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("StatusAt = %v, want the board's own field timestamp", p.StatusAt)
	}
}

func TestImportProjectBoardLeavesUnmappedStatusInert(t *testing.T) {
	board := newTestBoard(t)
	id := seedCard(t, board, 613, native.StateInbox)
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusValue("Icebox", time.Now().UTC())),
	}}}

	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, nil)
	if err != nil {
		t.Fatalf("ImportProjectBoard: %v", err)
	}
	if res.Moved != 0 {
		t.Errorf("an unmapped status must move nothing, Moved = %d", res.Moved)
	}
	if got := mustGet(t, board, id).State; got != native.StateInbox {
		t.Errorf("state = %q, want it untouched (%q)", got, native.StateInbox)
	}
}

// TestImportProjectBoardSkippedNoCardNamesTheRepos makes the skip
// ACTIONABLE: "12 skipped" tells an operator nothing, "8 in SocialGouv/iterion,
// 4 in SocialGouv/infra" tells them exactly which issue imports to run.
func TestImportProjectBoardSkippedNoCardNamesTheRepos(t *testing.T) {
	board := newTestBoard(t)
	at := time.Now().UTC()
	other := func(id, repo string, n int) forge.ProjectItem {
		it := item(id, n, statusValue("Planned", at))
		it.Content.Repo = repo
		return it
	}
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
		other("PVTI_1", "SocialGouv/iterion", 901),
		other("PVTI_2", "SocialGouv/iterion", 902),
		other("PVTI_3", "SocialGouv/infra", 903),
	}}}

	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, nil)
	if err != nil {
		t.Fatalf("ImportProjectBoard: %v", err)
	}
	if res.SkippedNoCard != 3 {
		t.Fatalf("SkippedNoCard = %d, want 3", res.SkippedNoCard)
	}
	if len(res.MissingRepos) != 2 {
		t.Fatalf("MissingRepos = %+v, want one entry per distinct repo", res.MissingRepos)
	}
	// Sorted: most-missing first, then by name, so the operator's first move is
	// the first line.
	if res.MissingRepos[0].Repo != "SocialGouv/iterion" || res.MissingRepos[0].Count != 2 {
		t.Errorf("MissingRepos[0] = %+v, want SocialGouv/iterion×2 first", res.MissingRepos[0])
	}
	if res.MissingRepos[1].Repo != "SocialGouv/infra" || res.MissingRepos[1].Count != 1 {
		t.Errorf("MissingRepos[1] = %+v", res.MissingRepos[1])
	}
}

func TestImportProjectBoardNeverCreatesCards(t *testing.T) {
	board := newTestBoard(t)
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
		item("PVTI_1", 999, statusValue("Planned", time.Now().UTC())),
	}}}

	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, nil)
	if err != nil {
		t.Fatalf("ImportProjectBoard: %v", err)
	}
	if res.SkippedNoCard != 1 {
		t.Errorf("SkippedNoCard = %d, want 1 (%+v)", res.SkippedNoCard, res)
	}
	all, err := board.List(native.ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("the project import must never create a card (author-trust runs at issue ingest), got %d", len(all))
	}
}

func TestImportProjectBoardSkipsDraftsAndPulls(t *testing.T) {
	board := newTestBoard(t)
	draft := forge.ProjectItem{ID: "PVTI_d", Content: forge.ProjectItemContent{Kind: forge.ProjectContentDraft, Title: "a thought"}}
	pull := forge.ProjectItem{ID: "PVTI_p", Content: forge.ProjectItemContent{
		Kind: forge.ProjectContentPull, Repo: "SocialGouv/iterion", Number: 1,
	}}
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{draft, pull}}}

	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, nil)
	if err != nil {
		t.Fatalf("ImportProjectBoard: %v", err)
	}
	if res.SkippedNoCard != 0 {
		t.Errorf("a draft/PR is not a missing card, it is not a card at all: %+v", res)
	}
	if res.Skipped != 2 {
		t.Errorf("Skipped = %d, want 2 (%+v)", res.Skipped, res)
	}
}

// TestImportProjectBoardSkipsArchivedItems pins the import half of the archive
// rule.
//
// GitHub PRESERVES an archived item's field values while removing it from
// every board view, so an item archived in "Planned" keeps reading as Planned
// forever. Driving a card's column from a value nobody can see or change is
// not a sync, and reflecting ONTO it writes into an invisible row. Both
// directions skip it, counted — silence would make an operator hunt for a card
// that stopped following for no stated reason.
func TestImportProjectBoardSkipsArchivedItems(t *testing.T) {
	board := newTestBoard(t)
	// The card moved natively AND the board says something else, so both
	// directions would have work to do were the item live.
	id := seedSynced(t, board, 613, native.StateInProgress, "Planned",
		time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC))
	archived := item("PVTI_1", 613, statusValue("Blocked", time.Now().UTC()))
	archived.Archived = true
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{archived}}}

	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board,
		&ProjectImportOptions{Binding: testBinding()})
	if err != nil {
		t.Fatalf("ImportProjectBoard: %v", err)
	}
	if res.SkippedArchived != 1 || res.Moved != 0 || res.Reflected != 0 || res.Conflicts != 0 {
		t.Errorf("result = %+v, want the item skipped as archived and nothing else touched", res)
	}
	if len(bc.writes) != 0 {
		t.Errorf("board writes = %+v, want none — an archived row is invisible to the operator", bc.writes)
	}
	if got := mustGet(t, board, id).State; got != native.StateInProgress {
		t.Errorf("state = %q, want it untouched by an archived item", got)
	}
}

// TestImportProjectBoardEchoSuppression is the loop guard: when the board's
// status is the one iterion itself last recorded, the import changes nothing —
// even though the card has since moved on. Without this the reflect's own
// write would come straight back and undo the transition that caused it.
func TestImportProjectBoardEchoSuppression(t *testing.T) {
	board := newTestBoard(t)
	id := seedCard(t, board, 613, native.StateDone)
	at := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	if _, err := board.Update(id, native.Patch{External: &native.ExternalRef{
		Provider: "github", Repo: "SocialGouv/iterion", Number: 613,
		Project: &native.ExternalProject{
			Owner: "SocialGouv", Number: 203, ItemID: "PVTI_1",
			Status: "In progress", StatusAt: at, StateAt: at,
		},
	}}); err != nil {
		t.Fatalf("seed sync state: %v", err)
	}
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusValue("In progress", at)),
	}}}

	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, nil)
	if err != nil {
		t.Fatalf("ImportProjectBoard: %v", err)
	}
	if res.Moved != 0 {
		t.Errorf("an unchanged board status must move nothing, Moved = %d", res.Moved)
	}
	if got := mustGet(t, board, id).State; got != native.StateDone {
		t.Errorf("state = %q, want %q — the card had moved on and must keep its state", got, native.StateDone)
	}
}

// TestImportProjectBoardConflictNewerWins pins the conflict rule against the
// CARD's own transition time, which the store stamps at every state write
// (native.Issue.StateAt). The board's timestamp is expressed as an offset from
// it, because "newer" is a comparison between the two sides and nothing else.
func TestImportProjectBoardConflictNewerWins(t *testing.T) {
	for _, tc := range []struct {
		name string
		// boardOffset positions the board's status timestamp relative to the
		// card's real transition.
		boardOffset       time.Duration
		wantState         string
		wantMoved, wantCf int
	}{
		{"github newer wins", time.Hour, native.StateInProgress, 1, 1},
		{"native newer wins", -time.Hour, native.StateReview, 0, 1},
		{"a tie goes to github", 0, native.StateInProgress, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			board := newTestBoard(t)
			id := seedCard(t, board, 613, native.StateReview)
			cardAt := mustGet(t, board, id).StateAt
			if cardAt.IsZero() {
				t.Fatal("the store must stamp a card's transition time; the rule has nothing to compare otherwise")
			}
			ghAt := cardAt.Add(tc.boardOffset)
			// The sync record carries a STALE write time — the value the rule
			// used to read. It must not decide anything here.
			if _, err := board.Update(id, native.Patch{External: &native.ExternalRef{
				Provider: "github", Repo: "SocialGouv/iterion", Number: 613,
				Project: &native.ExternalProject{
					Owner: "SocialGouv", Number: 203, ItemID: "PVTI_1",
					Status: "Planned", StatusAt: ghAt, StateAt: cardAt.Add(-24 * time.Hour),
				},
			}}); err != nil {
				t.Fatalf("seed sync state: %v", err)
			}
			bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
				item("PVTI_1", 613, statusValue("In progress", ghAt)),
			}}}

			res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, nil)
			if err != nil {
				t.Fatalf("ImportProjectBoard: %v", err)
			}
			if got := mustGet(t, board, id).State; got != tc.wantState {
				t.Errorf("state = %q, want %q", got, tc.wantState)
			}
			if res.Moved != tc.wantMoved {
				t.Errorf("Moved = %d, want %d", res.Moved, tc.wantMoved)
			}
			if res.Conflicts != tc.wantCf {
				t.Errorf("Conflicts = %d, want %d — every resolved conflict must be counted and logged", res.Conflicts, tc.wantCf)
			}
		})
	}
}

// TestImportProjectBoardRespectsTheTerminalSink pins that the board cannot
// resurrect a closed card. Leaving done/blocked is a REOPEN — an operator
// gesture with a dependents check and an audit trail — and automation never
// reopens (native/state_guard.go). Dragging a card out of Done on the forge
// board is reported, not silently obeyed and not silently dropped.
func TestImportProjectBoardRespectsTheTerminalSink(t *testing.T) {
	board := newTestBoard(t)
	id := seedCard(t, board, 613, native.StateDone)
	at := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusValue("Planned", at)),
	}}}

	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, nil)
	if err != nil {
		t.Fatalf("ImportProjectBoard: %v", err)
	}
	if got := mustGet(t, board, id).State; got != native.StateDone {
		t.Errorf("state = %q: automation must not reopen a terminal card", got)
	}
	if res.RefusedTerminal != 1 {
		t.Errorf("RefusedTerminal = %d, want 1 — a refused move must be reported, not dropped (%+v)", res.RefusedTerminal, res)
	}
	if res.Moved != 0 {
		t.Errorf("Moved = %d, want 0", res.Moved)
	}
}

func TestImportProjectBoardLabelReplacesSamePrefix(t *testing.T) {
	board := newTestBoard(t)
	id := seedCard(t, board, 613, native.StateInbox, "area:engine", "mode:direct", "kind:epic")
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613,
			selectValue("PVTSSF_area", "Area", "cloud/ops"),
			selectValue("PVTSSF_mode", "Mode", "dogfood"),
		),
	}}}

	if _, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, nil); err != nil {
		t.Fatalf("ImportProjectBoard: %v", err)
	}
	labels := mustGet(t, board, id).Labels
	for _, gone := range []string{"area:engine", "mode:direct"} {
		if slices.Contains(labels, gone) {
			t.Errorf("stale %q survived a field change: %v", gone, labels)
		}
	}
	for _, want := range []string{"area:cloud-ops", "mode:dogfood", "kind:epic"} {
		if !slices.Contains(labels, want) {
			t.Errorf("labels %v missing %q", labels, want)
		}
	}
}

func TestImportProjectBoardPaginates(t *testing.T) {
	board := newTestBoard(t)
	a := seedCard(t, board, 1, native.StateInbox)
	b := seedCard(t, board, 2, native.StateInbox)
	at := time.Now().UTC()
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{
		{item("PVTI_1", 1, statusValue("Planned", at))},
		{item("PVTI_2", 2, statusValue("Done", at))},
	}}

	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, nil)
	if err != nil {
		t.Fatalf("ImportProjectBoard: %v", err)
	}
	if res.Items != 2 || res.Moved != 2 {
		t.Fatalf("second page not consumed: %+v", res)
	}
	if got := mustGet(t, board, a).State; got != native.StateReady {
		t.Errorf("page 1 card state = %q", got)
	}
	if got := mustGet(t, board, b).State; got != native.StateDone {
		t.Errorf("page 2 card state = %q", got)
	}
}

func TestImportProjectBoardSurfacesClientErrors(t *testing.T) {
	board := newTestBoard(t)
	bc := &fakeBoardClient{err: forge.ErrProjectNotFound}
	if _, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, nil); !errors.Is(err, forge.ErrProjectNotFound) {
		t.Fatalf("want the client's error surfaced, got %v", err)
	}
}

// TestProjectLabelPrefixesAreBoardLocal is the class guard. The project labels
// are written by the project import and exist on no forge REPO, so a plain
// `iterion issue import` — which mirrors repo labels verbatim and keeps only
// board-local namespaces — would strip every one of them off every card.
func TestProjectLabelPrefixesAreBoardLocal(t *testing.T) {
	for _, lf := range forge.DefaultLabelFields() {
		label := lf.Prefix + "whatever"
		if !isBoardLocalLabel(label) {
			t.Errorf("%q must be board-local, or the next issue import strips it", label)
		}
		kept := mergeForgeLabels([]string{"bug"}, []string{label, "stale-forge-label"})
		if !slices.Contains(kept, label) {
			t.Errorf("mergeForgeLabels dropped %q: %v", label, kept)
		}
	}
}

// flakyBoard is a card store whose Get fails the way a Mongo blip does:
// a wrapped transient error, never the tracker.ErrNotFound sentinel. Every
// other method is the real store's (interface embedding).
type flakyBoard struct {
	native.BoardStore
	err error
}

func (f *flakyBoard) Get(id string) (*native.Issue, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.BoardStore.Get(id)
}

// TestImportProjectBoardPropagatesAStoreFailure pins that a store that is
// UNREACHABLE is not reported as a board whose issues were never imported.
//
// "No card for this item" is a specific fact — tracker.ErrNotFound on both
// twins — with a specific remedy the result spells out ("run the issue import
// for these repos first"). The Mongo twin also returns wrapped transient
// errors (mongoutil.FindOne wraps anything that is not ErrNoDocuments), and
// reading those as "no card" tells an operator to re-run an import that is
// already done while the real failure — the database — goes unmentioned.
func TestImportProjectBoardPropagatesAStoreFailure(t *testing.T) {
	board := newTestBoard(t)
	seedCard(t, board, 613, native.StateInbox)
	flaky := &flakyBoard{BoardStore: board, err: errors.New("boardmongo: get issue: connection reset by peer")}
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusValue("Planned", time.Now().UTC())),
	}}}

	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, flaky, nil)
	if err == nil {
		t.Fatalf("a store failure must surface, got nil (%+v)", res)
	}
	if !strings.Contains(err.Error(), "connection reset by peer") {
		t.Errorf("error = %v, want the store's own cause named", err)
	}
	if res.SkippedNoCard != 0 || len(res.MissingRepos) != 0 {
		t.Errorf("res = %+v, want no missing-import report — the import is not what failed", res)
	}
}

// And the sentinel keeps its meaning: a genuinely absent card is still
// counted and its repository named, not turned into a failed pass.
func TestImportProjectBoardStillReportsAMissingCard(t *testing.T) {
	board := newTestBoard(t)
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusValue("Planned", time.Now().UTC())),
	}}}

	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board, nil)
	if err != nil {
		t.Fatalf("a card that was never imported is not a failure: %v", err)
	}
	if res.SkippedNoCard != 1 || len(res.MissingRepos) != 1 || res.MissingRepos[0].Repo != "SocialGouv/iterion" {
		t.Errorf("res = %+v, want one skipped item naming its repo", res)
	}
}

// projectDriftingBoard moves a card once, just before the write that was decided on
// the state it read — the shape native.Store.SetStateFrom's CAS exists for.
// Its SetStateFrom then loses the CAS through the REAL store and answers
// (issue, changed=false, nil): a CAS loss is not an error.
type projectDriftingBoard struct {
	native.BoardStore
	driftTo string
	once    bool
}

func (d *projectDriftingBoard) SetStateFrom(id, from, to string) (*native.Issue, bool, error) {
	if !d.once {
		d.once = true
		if _, err := d.SetState(id, d.driftTo); err != nil {
			return nil, false, err
		}
	}
	return d.BoardStore.SetStateFrom(id, from, to)
}

// TestImportProjectBoardDoesNotCountACASLossAsAMove pins that a move the store
// REFUSED on its CAS is not recorded as applied.
//
// SetStateFrom answers a drifted card with (issue, false, nil) — the operator
// got there first — and the import discarded that flag, taking the nil error
// as success. It then counted a transition the store never made, and recorded
// the board's status as synchronized, which makes every later pass a no-op:
// on the one-shot `iterion issue import --project` path nothing ever repairs
// that, so the card stays where the operator put it while the record claims
// the board was applied.
func TestImportProjectBoardDoesNotCountACASLossAsAMove(t *testing.T) {
	board := newTestBoard(t)
	id := seedCard(t, board, 613, native.StateInbox)
	drifting := &projectDriftingBoard{BoardStore: board, driftTo: native.StateInProgress}
	bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
		item("PVTI_1", 613, statusValue("Planned", time.Now().UTC())),
	}}}

	res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, drifting, nil)
	if err != nil {
		t.Fatalf("ImportProjectBoard: %v", err)
	}
	if res.Moved != 0 {
		t.Errorf("Moved = %d, want 0 — the store refused the write (%+v)", res.Moved, res)
	}
	if got := mustGet(t, board, id).State; got != native.StateInProgress {
		t.Errorf("state = %q, want the operator's %q", got, native.StateInProgress)
	}
	// And nothing may claim the board's status was applied, or every later
	// pass reads "already equal" and the divergence becomes permanent.
	if ext := mustGet(t, board, id).External; ext != nil && ext.Project != nil && ext.Project.Status != "" {
		t.Errorf("recorded status = %q, want none — the write did not land", ext.Project.Status)
	}
}

// TestImportProjectBoardOneSidedMoveIsNotAConflict pins ADR-097 §9.2's own
// definition of the counter: items where BOTH sides had moved.
//
// Dragging a card is the ordinary gesture on a forge board, and the native
// side has NOT moved when the card still sits in the column iterion's own last
// recorded status put it in. Counting that as a conflict saturates the one
// number that answers "how often are people and bots fighting over this
// board". Worse, when the card's transition time happens to be the newer of
// the two, the phantom conflict resolves in the native side's favour and the
// reflect pushes the card's OLD column back over the human's drag.
func TestImportProjectBoardOneSidedMoveIsNotAConflict(t *testing.T) {
	for _, tc := range []struct {
		name string
		// cardState is where the card sits; recorded is the board status
		// iterion last synchronized — its mapped state IS iterion's own last
		// write, so cardState == map(recorded) means "the native side has not
		// moved".
		cardState, recorded, boardStatus string
		// boardOffset positions the board's status timestamp relative to the
		// card's real transition.
		boardOffset                            time.Duration
		wantState                              string
		wantMoved, wantReflected, wantConflict int
		wantWrites                             int
	}{
		{
			name: "only the board moved", cardState: native.StateReady,
			recorded: "Planned", boardStatus: "In progress", boardOffset: -time.Hour,
			wantState: native.StateInProgress, wantMoved: 1,
		},
		{
			name: "only the native board moved", cardState: native.StateInProgress,
			recorded: "Planned", boardStatus: "Planned", boardOffset: -time.Hour,
			wantState: native.StateInProgress, wantReflected: 1, wantWrites: 1,
		},
		{
			name: "both moved", cardState: native.StateInProgress,
			recorded: "Planned", boardStatus: "Blocked", boardOffset: time.Hour,
			wantState: native.StateBlocked, wantMoved: 1, wantConflict: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			board := newTestBoard(t)
			id := seedSynced(t, board, 613, tc.cardState, tc.recorded, time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC))
			ghAt := cardStateAt(t, board, id).Add(tc.boardOffset)
			bc := &fakeBoardClient{project: testProject(), pages: [][]forge.ProjectItem{{
				item("PVTI_1", 613, statusValue(tc.boardStatus, ghAt)),
			}}}

			res, err := ImportProjectBoard(context.Background(), bc, testProjectRef, forge.ProviderGitHub, board,
				&ProjectImportOptions{Binding: testBinding()})
			if err != nil {
				t.Fatalf("ImportProjectBoard: %v", err)
			}
			if got := mustGet(t, board, id).State; got != tc.wantState {
				t.Errorf("state = %q, want %q", got, tc.wantState)
			}
			if res.Moved != tc.wantMoved || res.Reflected != tc.wantReflected {
				t.Errorf("Moved/Reflected = %d/%d, want %d/%d (%+v)",
					res.Moved, res.Reflected, tc.wantMoved, tc.wantReflected, res)
			}
			if res.Conflicts != tc.wantConflict {
				t.Errorf("Conflicts = %d, want %d — the counter means BOTH sides moved", res.Conflicts, tc.wantConflict)
			}
			if len(bc.writes) != tc.wantWrites {
				t.Errorf("board writes = %+v, want %d — a one-sided board move must never be pushed back",
					bc.writes, tc.wantWrites)
			}
		})
	}
}
