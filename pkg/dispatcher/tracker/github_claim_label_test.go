package tracker_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

func ghCallsOf(fake *fakeGH, first, second string) [][]string {
	var out [][]string
	for _, c := range fake.calls {
		if len(c) > 1 && c[0] == first && c[1] == second {
			out = append(out, c)
		}
	}
	return out
}

// TestGitHubClaim_CreatesTheClaimedLabelOnAFreshRepo: `gh issue edit
// --add-label` REFUSES a label the repository does not carry ('<label>'
// not found), so on a fresh repo EVERY issue failed its claim at every
// tick, for ever — warned each time, repaired never. The Forgejo twin
// creates the missing label (resolveLabelID, "so a fresh repository can
// be dispatched without manual label setup"); the GitHub adapter must
// too, once per adapter, and only when the repo lacks it (never
// --force: an operator's colour on an existing label is theirs).
func TestGitHubClaim_CreatesTheClaimedLabelOnAFreshRepo(t *testing.T) {
	fake := &fakeGH{} // an empty repository: `gh label list` answers []
	a := newGHAdapter(t, fake, nil)
	if err := a.Claim(context.Background(), "github:owner/repo#5", "h-1"); err != nil {
		t.Fatalf("Claim on a fresh repo: %v", err)
	}
	creates := ghCallsOf(fake, "label", "create")
	if len(creates) != 1 || !contains(creates[0], "iterion-claimed") {
		t.Fatalf("REPRODUCED: the claim never created the missing label (label create calls: %v) — every "+
			"claim on this repo fails at every tick", creates)
	}
	if contains(creates[0], "--force") {
		t.Fatalf("label create must not --force: it would overwrite an operator's colour/description on an existing label: %v", creates[0])
	}
	// Ordering: the label exists BEFORE the first --add-label.
	var sawCreate bool
	for _, c := range fake.calls {
		if len(c) > 1 && c[0] == "label" && c[1] == "create" {
			sawCreate = true
		}
		if len(c) > 1 && c[0] == "issue" && c[1] == "edit" && contains(c, "--add-label") && !sawCreate {
			t.Fatalf("--add-label ran before the label was created: %v", fake.calls)
		}
	}
	// Memoised: a second claim neither lists nor creates again.
	if err := a.Claim(context.Background(), "github:owner/repo#6", "h-1"); err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if n := len(ghCallsOf(fake, "label", "create")); n != 1 {
		t.Fatalf("label create ran %d times across two claims, want 1 (memoised per adapter)", n)
	}
	if n := len(ghCallsOf(fake, "label", "list")); n != 1 {
		t.Fatalf("label list ran %d times across two claims, want 1 (memoised per adapter)", n)
	}
}

// A repository that already carries the label is left alone: no create.
func TestGitHubClaim_DoesNotRecreateAnExistingLabel(t *testing.T) {
	fake := &fakeGH{labelListOut: []byte(`[{"name":"bug"},{"name":"iterion-claimed"}]`)}
	a := newGHAdapter(t, fake, nil)
	if err := a.Claim(context.Background(), "github:owner/repo#5", "h-1"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if n := len(ghCallsOf(fake, "label", "create")); n != 0 {
		t.Fatalf("label create ran %d times on a repo that has the label", n)
	}
}

// A label the adapter cannot create is a claim that cannot happen: the
// error surfaces (loudly, typed by text) instead of an --add-label that
// would fail with a less useful message.
func TestGitHubClaim_LabelCreateFailureIsLoud(t *testing.T) {
	fake := &fakeGH{labelCreateErr: errors.New("HTTP 403: Resource not accessible by integration")}
	a := newGHAdapter(t, fake, nil)
	err := a.Claim(context.Background(), "github:owner/repo#5", "h-1")
	if err == nil || !strings.Contains(err.Error(), "iterion-claimed") || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a failed label bootstrap must surface with the label and the cause, got %v", err)
	}
	if n := len(ghCallsOf(fake, "issue", "edit")); n != 0 {
		t.Fatalf("no --add-label may be attempted when the label could not be ensured, got %d edits", n)
	}
	// Not memoised as ensured: the next claim tries again.
	fake.labelCreateErr = nil
	if err := a.Claim(context.Background(), "github:owner/repo#5", "h-1"); err != nil {
		t.Fatalf("Claim after the bootstrap recovers: %v", err)
	}
}

// The label deleted behind the adapter's back (an operator tidying the
// repo's labels) must not put the repo back into the every-tick failure:
// the memo is dropped, the label re-created, the claim retried once.
func TestGitHubClaim_RecreatesALabelDeletedMidLife(t *testing.T) {
	fake := &fakeGH{labelListOut: []byte(`[{"name":"iterion-claimed"}]`)}
	a := newGHAdapter(t, fake, nil)
	if err := a.Claim(context.Background(), "github:owner/repo#1", "h-1"); err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	// Someone deletes the label; gh 2.x refuses the next --add-label.
	fake.labelListOut = []byte(`[]`)
	fake.editErrOnce = errors.New("failed to update https://github.com/owner/repo/issues/2: 'iterion-claimed' not found\nfailed to update 1 issue")
	if err := a.Claim(context.Background(), "github:owner/repo#2", "h-1"); err != nil {
		t.Fatalf("Claim after the label vanished: %v", err)
	}
	if n := len(ghCallsOf(fake, "label", "create")); n != 1 {
		t.Fatalf("the vanished label must be re-created exactly once, got %d creates", n)
	}
	if n := len(ghCallsOf(fake, "issue", "edit")); n != 3 {
		t.Fatalf("want 3 edits (first claim, refused claim, retried claim), got %d: %v", n, fake.calls)
	}
	// An unrelated edit failure is NOT a missing-label signal: no retry,
	// no create, the error surfaces.
	fake.editErrOnce = errors.New("HTTP 502: bad gateway")
	if err := a.Claim(context.Background(), "github:owner/repo#3", "h-1"); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("an unrelated failure must surface unchanged, got %v", err)
	}
	if n := len(ghCallsOf(fake, "label", "create")); n != 1 {
		t.Fatalf("an unrelated failure must not re-create the label, got %d creates", n)
	}
}

var _ tracker.Tracker = (*tracker.GitHubAdapter)(nil)
