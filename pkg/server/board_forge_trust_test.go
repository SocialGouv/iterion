package server

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/forge"
)

func TestUpsertForgeCard_TrustStamping(t *testing.T) {
	board := newTestBoard(t)
	b := board.Board()
	openCol, doneCol := defaultOpenColumn(b), terminalColumn(b)

	// Trusted author → triage:auto on the fresh open card.
	trustedIs := forge.IssueRef{Number: 1, Title: "t", State: "open", Labels: []string{"bug"}, Author: "alice"}
	if _, _, err := upsertForgeCard(board, b, openCol, doneCol, forge.ProviderGitHub, "c", "o/r", trustedIs, true); err != nil {
		t.Fatalf("upsert trusted: %v", err)
	}
	card, _ := board.Get(forgeCardID(forge.ProviderGitHub, "o/r", 1))
	if !slices.Contains(card.Labels, native.LabelTriageAuto) {
		t.Fatalf("trusted create missing %s: %v", native.LabelTriageAuto, card.Labels)
	}
	if card.External == nil || card.External.Author != "alice" {
		t.Fatalf("author not stamped on external ref: %+v", card.External)
	}

	// Untrusted author → needs:approval, never triage:auto.
	extIs := forge.IssueRef{Number: 2, Title: "x", State: "open", Author: "drive-by"}
	if _, _, err := upsertForgeCard(board, b, openCol, doneCol, forge.ProviderGitHub, "c", "o/r", extIs, false); err != nil {
		t.Fatalf("upsert untrusted: %v", err)
	}
	card, _ = board.Get(forgeCardID(forge.ProviderGitHub, "o/r", 2))
	if !slices.Contains(card.Labels, native.LabelNeedsApproval) || slices.Contains(card.Labels, native.LabelTriageAuto) {
		t.Fatalf("untrusted create labels wrong: %v", card.Labels)
	}

	// Closed issue → no trust stamp at all.
	closedIs := forge.IssueRef{Number: 3, Title: "old", State: "closed", Author: "alice"}
	if _, _, err := upsertForgeCard(board, b, openCol, doneCol, forge.ProviderGitHub, "c", "o/r", closedIs, true); err != nil {
		t.Fatalf("upsert closed: %v", err)
	}
	card, _ = board.Get(forgeCardID(forge.ProviderGitHub, "o/r", 3))
	if slices.Contains(card.Labels, native.LabelTriageAuto) || slices.Contains(card.Labels, native.LabelNeedsApproval) {
		t.Fatalf("closed create must not stamp trust labels: %v", card.Labels)
	}

	// Update never RE-stamps: consume triage:auto (as the trigger does), then
	// re-sync — the label must stay gone, while board-local labels survive.
	id := forgeCardID(forge.ProviderGitHub, "o/r", 1)
	consumed := []string{"bug", "source:issue-triage", "needs-manual-triage"}
	if _, err := board.Update(id, native.Patch{Labels: &consumed}); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if _, _, err := upsertForgeCard(board, b, openCol, doneCol, forge.ProviderGitHub, "c", "o/r", trustedIs, true); err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	card, _ = board.Get(id)
	if slices.Contains(card.Labels, native.LabelTriageAuto) {
		t.Fatalf("re-sync re-stamped the consumed trigger label: %v", card.Labels)
	}
	for _, want := range []string{"bug", "source:issue-triage", "needs-manual-triage"} {
		if !slices.Contains(card.Labels, want) {
			t.Fatalf("re-sync lost label %q: %v", want, card.Labels)
		}
	}
}

func TestMergeForgeLabels(t *testing.T) {
	got := mergeForgeLabels(
		[]string{"bug", "P1"},
		[]string{"bug", "triage:auto", "needs:approval", "cmd:revi-1", "source:whats-next", "stale-forge-label"},
	)
	want := []string{"bug", "P1", "triage:auto", "needs:approval", "cmd:revi-1", "source:whats-next"}
	if !slices.Equal(got, want) {
		t.Fatalf("merge = %v, want %v", got, want)
	}
	// A forge label removed on the forge disappears; case-insensitive dedup.
	got = mergeForgeLabels([]string{"Bug"}, []string{"bug", "helpme"})
	if !slices.Equal(got, []string{"Bug"}) {
		t.Fatalf("merge = %v, want [Bug]", got)
	}
}

// TestSyncForgeIssuesToBoard_NilTrustFailsClosed: no trust classifier ⇒ every
// fresh open card parks as needs:approval (the security default).
func TestSyncForgeIssuesToBoard_NilTrustFailsClosed(t *testing.T) {
	board := newTestBoard(t)
	ic := &fakeIssueClient{issues: []forge.IssueRef{
		{Number: 1, Title: "a", State: "open", Author: "whoever"},
	}}
	if _, _, err := syncForgeIssuesToBoard(context.Background(), ic, forge.ProviderGitHub, "c", "o/r", board, time.Time{}, nil); err != nil {
		t.Fatalf("sync: %v", err)
	}
	card, _ := board.Get(forgeCardID(forge.ProviderGitHub, "o/r", 1))
	if !slices.Contains(card.Labels, native.LabelNeedsApproval) {
		t.Fatalf("nil trust must park: %v", card.Labels)
	}
}

// TestSyncForgeIssuesToBoard_TrustMemoized: one classification per distinct
// author per sweep.
func TestSyncForgeIssuesToBoard_TrustMemoized(t *testing.T) {
	board := newTestBoard(t)
	ic := &fakeIssueClient{issues: []forge.IssueRef{
		{Number: 1, Title: "a", State: "open", Author: "alice"},
		{Number: 2, Title: "b", State: "open", Author: "alice"},
		{Number: 3, Title: "c", State: "open", Author: "bob"},
	}}
	calls := map[string]int{}
	trust := func(_ context.Context, login string) bool {
		calls[login]++
		return login == "alice"
	}
	if _, _, err := syncForgeIssuesToBoard(context.Background(), ic, forge.ProviderGitHub, "c", "o/r", board, time.Time{}, trust); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if calls["alice"] != 1 || calls["bob"] != 1 {
		t.Fatalf("trust calls not memoized: %v", calls)
	}
	aliceCard, _ := board.Get(forgeCardID(forge.ProviderGitHub, "o/r", 1))
	bobCard, _ := board.Get(forgeCardID(forge.ProviderGitHub, "o/r", 3))
	if !slices.Contains(aliceCard.Labels, native.LabelTriageAuto) {
		t.Fatalf("alice card: %v", aliceCard.Labels)
	}
	if !slices.Contains(bobCard.Labels, native.LabelNeedsApproval) {
		t.Fatalf("bob card: %v", bobCard.Labels)
	}
}
