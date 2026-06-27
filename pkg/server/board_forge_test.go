package server

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
)

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
	c, u, err := upsertForgeCard(board, b, openCol, doneCol, forge.ProviderGitHub, "conn1", "org/api", is)
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
	c, u, err = upsertForgeCard(board, b, openCol, doneCol, forge.ProviderGitHub, "conn1", "org/api", is)
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
	if _, _, err := upsertForgeCard(board, b, openCol, doneCol, forge.ProviderGitHub, "conn1", "org/api", is); err != nil {
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

func TestUpsertForgeCard_ClosedCreatesInTerminal(t *testing.T) {
	board := newTestBoard(t)
	b := board.Board()
	is := forge.IssueRef{Number: 9, Title: "old", State: "closed"}
	if _, _, err := upsertForgeCard(board, b, defaultOpenColumn(b), terminalColumn(b), forge.ProviderForgejo, "c", "o/r", is); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	card, _ := board.Get(forgeCardID(forge.ProviderForgejo, "o/r", 9))
	if card.State != terminalColumn(b) {
		t.Errorf("closed issue should be created in terminal column, got %q", card.State)
	}
}
