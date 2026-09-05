package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
)

// fakeIssueClient is a minimal forge.IssueClient whose ListIssues returns a
// canned set; the other methods are unused by SyncForgeIssuesToBoard.
type fakeIssueClient struct {
	issues []forge.IssueRef
	calls  int
}

func (f *fakeIssueClient) ListIssues(context.Context, string, forge.IssueListOptions) ([]forge.IssueRef, error) {
	f.calls++
	return f.issues, nil
}
func (f *fakeIssueClient) GetIssue(context.Context, string, int) (forge.IssueRef, error) {
	return forge.IssueRef{}, nil
}
func (f *fakeIssueClient) CreateIssue(context.Context, string, forge.NewIssue) (forge.IssueRef, error) {
	return forge.IssueRef{}, nil
}
func (f *fakeIssueClient) UpdateIssue(context.Context, string, int, forge.IssuePatch) (forge.IssueRef, error) {
	return forge.IssueRef{}, nil
}
func (f *fakeIssueClient) CommentIssue(context.Context, string, int, string) (forge.CommentRef, error) {
	return forge.CommentRef{}, nil
}

// newTestBoard returns a fresh filesystem board store with the default layout
// (inbox … done[terminal]).
func newTestBoard(t *testing.T) *native.Store {
	t.Helper()
	st, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestForgeCardID_Deterministic(t *testing.T) {
	a := forgeCardID(forge.ProviderGitHub, "org/api", 12)
	b := forgeCardID(forge.ProviderGitHub, "org/api", 12)
	if a != b {
		t.Fatalf("card id not deterministic: %q != %q", a, b)
	}
	if forgeCardID(forge.ProviderGitHub, "org/api", 13) == a {
		t.Fatal("different issue numbers must yield different ids")
	}
	// Must be a valid native id (native:<uuid>) the store accepts on Create.
	board := newTestBoard(t)
	if _, err := board.Create(native.Issue{ID: a, Title: "x", State: "inbox"}); err != nil {
		t.Fatalf("store rejected deterministic card id %q: %v", a, err)
	}
}

func TestUpsertForgeCard_CreateUpdateIdempotent(t *testing.T) {
	board := newTestBoard(t)
	b := board.Board()
	openCol := defaultOpenColumn(b) // "inbox"
	doneCol := terminalColumn(b)    // "done"
	if openCol == "" || doneCol == "" {
		t.Fatalf("default board lacks open/terminal column: open=%q done=%q", openCol, doneCol)
	}

	is := forge.IssueRef{Number: 7, Title: "fix login", Body: "boom", State: "open", URL: "http://f/7", Labels: []string{"bug"}}
	c, u, err := upsertForgeCard(board, b, openCol, doneCol, forge.ProviderGitHub, "conn1", "org/api", is, false)
	if err != nil || c != 1 || u != 0 {
		t.Fatalf("create: c=%d u=%d err=%v", c, u, err)
	}
	id := forgeCardID(forge.ProviderGitHub, "org/api", 7)
	card, err := board.Get(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if card.State != openCol {
		t.Errorf("open issue should land in %q, got %q", openCol, card.State)
	}
	if card.External == nil || card.External.Repo != "org/api" {
		t.Fatalf("forge external ref not stamped: %+v", card.External)
	}
	if card.External.Number != 7 || card.External.ConnectionID != "conn1" {
		t.Errorf("external ref wrong: %+v", card.External)
	}

	// Operator moves the card forward; a content re-sync must NOT yank it back.
	if _, err := board.SetState(id, "in_progress"); err != nil {
		t.Fatalf("setstate: %v", err)
	}
	is.Title = "fix login (v2)"
	c, u, err = upsertForgeCard(board, b, openCol, doneCol, forge.ProviderGitHub, "conn1", "org/api", is, false)
	if err != nil || c != 0 || u != 1 {
		t.Fatalf("update: c=%d u=%d err=%v", c, u, err)
	}
	card, _ = board.Get(id)
	if card.Title != "fix login (v2)" {
		t.Errorf("title not updated: %q", card.Title)
	}
	if card.State != "in_progress" {
		t.Errorf("re-sync clobbered operator column: %q", card.State)
	}

	// Forge closes the issue → still-open card moves to the terminal column.
	is.State = "closed"
	if _, _, err := upsertForgeCard(board, b, openCol, doneCol, forge.ProviderGitHub, "conn1", "org/api", is, false); err != nil {
		t.Fatalf("close-sync: %v", err)
	}
	card, _ = board.Get(id)
	if card.State != doneCol {
		t.Errorf("closed forge issue should move card to %q, got %q", doneCol, card.State)
	}

	// Whole sweep must not have duplicated the card.
	all, _ := board.List(native.ListFilter{})
	if len(all) != 1 {
		t.Errorf("expected exactly 1 card after idempotent upserts, got %d", len(all))
	}
}

// TestSyncForgeIssuesToBoard_StoreAgnostic exercises the extracted pure core
// against a fake issue client + a local native store — the shape a self-hosted
// import (no cloud integration store) would use. Asserts open issues land in
// the first column, PRs are skipped, and a re-sync upserts (no duplicates).
func TestSyncForgeIssuesToBoard_StoreAgnostic(t *testing.T) {
	board := newTestBoard(t)
	openCol := defaultOpenColumn(board.Board())
	ic := &fakeIssueClient{issues: []forge.IssueRef{
		{Number: 1, Title: "add metrics", State: "open", Labels: []string{"feat"}},
		{Number: 2, Title: "a PR, skipped", State: "open", IsPullRequest: true},
		{Number: 3, Title: "old bug", State: "closed"},
	}}

	created, updated, err := syncForgeIssuesToBoard(
		context.Background(), ic, forge.ProviderGitHub, "conn1", "org/api", board, time.Time{}, nil)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if created != 2 || updated != 0 {
		t.Fatalf("first sync created=%d updated=%d, want 2/0 (PR skipped)", created, updated)
	}
	all, _ := board.List(native.ListFilter{})
	if len(all) != 2 {
		t.Fatalf("expected 2 cards (PR excluded), got %d", len(all))
	}
	openCard, err := board.Get(forgeCardID(forge.ProviderGitHub, "org/api", 1))
	if err != nil {
		t.Fatalf("get open card: %v", err)
	}
	if openCard.State != openCol {
		t.Errorf("open issue landed in %q, want first column %q", openCard.State, openCol)
	}

	// Re-sync = idempotent upsert, not duplication.
	created, updated, err = syncForgeIssuesToBoard(
		context.Background(), ic, forge.ProviderGitHub, "conn1", "org/api", board, time.Time{}, nil)
	if err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	if created != 0 || updated != 2 {
		t.Fatalf("re-sync created=%d updated=%d, want 0/2", created, updated)
	}
	if all, _ := board.List(native.ListFilter{}); len(all) != 2 {
		t.Fatalf("re-sync duplicated cards: got %d, want 2", len(all))
	}
	if ic.calls != 2 {
		t.Fatalf("ListIssues called %d times, want 2 (one per sync)", ic.calls)
	}
}

// TestImportForgeIssues_UnsupportedProvider verifies the self-hosted wrapper's
// provider switch rejects an unknown forge before any network call — the core
// idempotent-upsert behaviour is covered by TestSyncForgeIssuesToBoard_StoreAgnostic
// (ImportForgeIssues is a thin construct-then-delegate shim over it).
func TestImportForgeIssues_UnsupportedProvider(t *testing.T) {
	board := newTestBoard(t)
	_, _, err := ImportForgeIssues(context.Background(), forge.Provider("bitbucket"), "", "tok", "org/api", board, time.Time{}, "")
	if err == nil {
		t.Fatal("expected an error for an unsupported provider, got nil")
	}
}

func TestUpsertForgeCard_ClosedCreatesInTerminal(t *testing.T) {
	board := newTestBoard(t)
	b := board.Board()
	is := forge.IssueRef{Number: 9, Title: "old", State: "closed"}
	if _, _, err := upsertForgeCard(board, b, defaultOpenColumn(b), terminalColumn(b), forge.ProviderForgejo, "c", "o/r", is, false); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	card, _ := board.Get(forgeCardID(forge.ProviderForgejo, "o/r", 9))
	if card.State != terminalColumn(b) {
		t.Errorf("closed issue should be created in terminal column, got %q", card.State)
	}
}

// TestUpsertForgeCard_PropagatesAStoreFailure is the issue import's half of the
// same class as the project import's: reading ANY board.Get error as "no card
// yet" turns a store outage into a CREATE of a card that already exists. Both
// twins answer a missing card with tracker.ErrNotFound and wrap everything
// else, so the sentinel is what distinguishes the two.
func TestUpsertForgeCard_PropagatesAStoreFailure(t *testing.T) {
	board := newTestBoard(t)
	b := board.Board()
	flaky := &flakyBoard{BoardStore: board, err: errors.New("boardmongo: get issue: i/o timeout")}

	is := forge.IssueRef{Number: 7, Title: "fix login", State: "open"}
	_, _, err := upsertForgeCard(flaky, b, defaultOpenColumn(b), terminalColumn(b),
		forge.ProviderGitHub, "conn1", "org/api", is, false)
	if err == nil {
		t.Fatal("a store failure must surface, got nil — the card was created blind")
	}
	if !strings.Contains(err.Error(), "i/o timeout") {
		t.Errorf("error = %v, want the store's own cause named", err)
	}
}
