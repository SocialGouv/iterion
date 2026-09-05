package tracker_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// fakeGH is a Command stub that returns canned JSON per gh subcommand.
type fakeGH struct {
	mu           sync.Mutex
	listOut      []byte
	apiOut       []byte            // legacy: response for `gh api ...` when apiOutByCall is unset
	apiOutByCall map[string][]byte // per-endpoint canned responses (key is the resource path)
	calls        [][]string
	failNum      int
	editErr      error
}

func (f *fakeGH) cmd(_ context.Context, args []string, _ []string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), args...))
	if len(args) == 0 {
		return nil, errors.New("no args")
	}
	switch {
	case args[0] == "issue" && args[1] == "list":
		return f.listOut, nil
	case args[0] == "api":
		if f.apiOutByCall != nil && len(args) >= 2 {
			// args[1] is the resource (may include `?...`); match on
			// the path-only prefix.
			path := args[1]
			if i := strings.Index(path, "?"); i >= 0 {
				path = path[:i]
			}
			if out, ok := f.apiOutByCall[path]; ok {
				return out, nil
			}
			return nil, fmt.Errorf("no canned response for path %q", path)
		}
		return f.apiOut, nil
	case args[0] == "issue" && (args[1] == "edit" || args[1] == "comment"):
		if f.editErr != nil {
			return nil, f.editErr
		}
		if f.failNum != 0 {
			var n int
			_, _ = fmt.Sscanf(args[2], "%d", &n)
			if n == f.failNum {
				return nil, errors.New("simulated failure")
			}
		}
		return nil, nil
	}
	return nil, fmt.Errorf("unhandled args: %v", args)
}

func newGHAdapter(t *testing.T, fake *fakeGH, mapping map[string]tracker.LabelSelector) *tracker.GitHubAdapter {
	t.Helper()
	a, err := tracker.NewGitHub(tracker.GitHubOptions{
		Repo:         "owner/repo",
		StateMapping: mapping,
		Command:      fake.cmd,
	})
	if err != nil {
		t.Fatalf("NewGitHub: %v", err)
	}
	return a
}

func TestGitHubListCandidates(t *testing.T) {
	fake := &fakeGH{
		listOut: mustJSON([]map[string]any{
			{
				"number":    42,
				"title":     "fix the bug",
				"body":      "body",
				"state":     "open",
				"labels":    []map[string]string{{"name": "ready"}},
				"createdAt": "2026-05-01T00:00:00Z",
				"updatedAt": "2026-05-01T00:00:00Z",
				"url":       "https://github.com/owner/repo/issues/42",
			},
			{
				// no matching label → filtered out
				"number":    99,
				"title":     "untriaged",
				"state":     "open",
				"labels":    []map[string]string{{"name": "noise"}},
				"createdAt": "2026-05-02T00:00:00Z",
				"updatedAt": "2026-05-02T00:00:00Z",
				"url":       "https://github.com/owner/repo/issues/99",
			},
		}),
	}
	a := newGHAdapter(t, fake, map[string]tracker.LabelSelector{
		"ready": {LabelsInclude: []string{"ready"}},
	})
	got, err := a.ListCandidates(context.Background())
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 issue, got %d", len(got))
	}
	if got[0].ID != "github:owner/repo#42" {
		t.Fatalf("ID: %s", got[0].ID)
	}
	if got[0].WorkflowState != "ready" {
		t.Fatalf("state: %s", got[0].WorkflowState)
	}
	if got[0].Metadata["url"] != "https://github.com/owner/repo/issues/42" {
		t.Fatalf("url metadata: %s", got[0].Metadata["url"])
	}
	// The claim filter is deliberately NOT server-side: excluding it from the
	// search would also hide claimed issues from the open-issue set the
	// dependency gate resolves blockers against. Filtered client-side instead.
	if strings.Contains(strings.Join(fake.calls[0], " "), "-label:iterion-claimed") {
		t.Fatalf("claim filter must not be server-side (it would hide in-flight blockers): %v", fake.calls[0])
	}
}

// A claimed issue is being implemented RIGHT NOW, which is the single case a
// dependency gate exists for. Excluding claimed issues server-side dropped
// them from openNums, so HeldByOpenBlockers treated the blocker as satisfied
// and released the dependent issue exactly then.
func TestGitHubClaimedIssueStillHoldsItsDependents(t *testing.T) {
	fake := &fakeGH{
		listOut: mustJSON([]map[string]any{
			{
				"number": 7, "title": "the blocker", "state": "OPEN",
				"labels":    []map[string]string{{"name": "ready"}, {"name": "iterion-claimed"}},
				"createdAt": "2026-05-01T00:00:00Z", "updatedAt": "2026-05-01T00:00:00Z",
			},
			{
				"number": 42, "title": "the dependent", "body": "Blocked by #7", "state": "OPEN",
				"labels":    []map[string]string{{"name": "ready"}},
				"createdAt": "2026-05-01T00:00:00Z", "updatedAt": "2026-05-01T00:00:00Z",
			},
		}),
	}
	a := newGHAdapter(t, fake, map[string]tracker.LabelSelector{
		"ready": {LabelsInclude: []string{"ready"}},
	})
	got, err := a.ListCandidates(context.Background())
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	// The claimed blocker is not a candidate itself, and it holds #42.
	for _, iss := range got {
		if strings.HasSuffix(iss.ID, "#7") {
			t.Errorf("a claimed issue must not be a candidate: %s", iss.ID)
		}
		if strings.HasSuffix(iss.ID, "#42") {
			t.Errorf("dependent dispatched while its blocker #7 is in flight: %s", iss.ID)
		}
	}
}

func TestGitHubResolveStateOrder(t *testing.T) {
	fake := &fakeGH{
		listOut: mustJSON([]map[string]any{
			{"number": 1, "labels": []map[string]string{{"name": "ready"}, {"name": "claimed"}}, "title": "x", "createdAt": "2026-05-01T00:00:00Z", "updatedAt": "2026-05-01T00:00:00Z"},
		}),
	}
	a := newGHAdapter(t, fake, map[string]tracker.LabelSelector{
		"in_progress": {LabelsInclude: []string{"claimed"}},
		"ready":       {LabelsInclude: []string{"ready"}, LabelsExclude: []string{"claimed"}},
	})
	got, _ := a.ListCandidates(context.Background())
	// in_progress matches because the issue has "claimed". Sorted state names: "in_progress" < "ready".
	if got[0].WorkflowState != "in_progress" {
		t.Fatalf("state: %s", got[0].WorkflowState)
	}
}

func TestGitHubRefreshStates(t *testing.T) {
	// RefreshStates now fetches each wanted issue individually (one
	// `gh api repos/<r>/issues/<n>` per ID) rather than scanning the
	// whole repo. This avoids silently truncating at per_page=100 when
	// a running issue is on page 2+. The fake returns a SINGLE-issue
	// REST shape on every call, gated on the requested number.
	fake := &fakeGH{
		apiOutByCall: map[string][]byte{
			"repos/owner/repo/issues/7": mustJSON(map[string]any{
				"number":     7,
				"title":      "x",
				"labels":     []map[string]string{{"name": "ready"}},
				"state":      "open",
				"created_at": "2026-05-01T00:00:00Z",
				"updated_at": "2026-05-01T00:00:00Z",
			}),
			// #9999 not configured → fake returns empty/error → skipped.
		},
	}
	a := newGHAdapter(t, fake, map[string]tracker.LabelSelector{
		"ready": {LabelsInclude: []string{"ready"}},
	})
	got, err := a.RefreshStates(context.Background(), []string{"github:owner/repo#7", "github:owner/repo#9999", "bogus"})
	if err != nil {
		t.Fatalf("RefreshStates: %v", err)
	}
	if got["github:owner/repo#7"] != "ready" {
		t.Fatalf("state for #7: %s", got["github:owner/repo#7"])
	}
	if _, ok := got["github:owner/repo#9999"]; ok {
		t.Fatal("missing ID should be omitted")
	}
}

func TestGitHubClaimAndRelease(t *testing.T) {
	fake := &fakeGH{}
	a := newGHAdapter(t, fake, nil)
	if err := a.Claim(context.Background(), "github:owner/repo#5", "h-1"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := a.Release(context.Background(), "github:owner/repo#5", "h-1"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// Last two calls should be edit --add-label, edit --remove-label.
	if !contains(fake.calls[0], "--add-label") || !contains(fake.calls[1], "--remove-label") {
		t.Fatalf("unexpected calls: %v", fake.calls)
	}
}

func TestGitHubUpdateStateMissingMapping(t *testing.T) {
	fake := &fakeGH{}
	a := newGHAdapter(t, fake, map[string]tracker.LabelSelector{
		"ready": {LabelsInclude: []string{"ready"}},
	})
	err := a.UpdateState(context.Background(), "github:owner/repo#1", "noplace")
	if !errors.Is(err, tracker.ErrTransitionRejected) {
		t.Fatalf("want ErrTransitionRejected, got %v", err)
	}
}

func TestGitHubInvalidID(t *testing.T) {
	fake := &fakeGH{}
	a := newGHAdapter(t, fake, nil)
	if err := a.Claim(context.Background(), "native:abc", "h"); !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestGitHubCmdFailureBubblesUp(t *testing.T) {
	fake := &fakeGH{failNum: 8}
	a := newGHAdapter(t, fake, nil)
	err := a.Claim(context.Background(), "github:owner/repo#8", "h")
	if err == nil || !strings.Contains(err.Error(), "simulated failure") {
		t.Fatalf("expected wrapped failure, got %v", err)
	}
}

// Compile-time assertion: *GitHubAdapter satisfies tracker.Tracker.
var _ tracker.Tracker = (*tracker.GitHubAdapter)(nil)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func mustJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// TestGitHubListCandidatesBlockers proves the body-declared dependency gate:
// a "Blocked by #N" issue is held while #N is open, and dispatched once #N is
// gone (fail-open — an unresolvable ref never stalls the issue).
func TestGitHubListCandidatesBlockers(t *testing.T) {
	mapping := map[string]tracker.LabelSelector{"ready": {LabelsInclude: []string{"ready"}}}
	dependent := map[string]any{
		"number": 42, "title": "needs the schema", "body": "Blocked by #7",
		"state": "open", "labels": []map[string]string{{"name": "ready"}},
		"createdAt": "2026-05-01T00:00:00Z", "updatedAt": "2026-05-01T00:00:00Z",
		"url": "https://github.com/owner/repo/issues/42",
	}

	t.Run("held while blocker open", func(t *testing.T) {
		blocker := map[string]any{
			"number": 7, "title": "create schema", "state": "open",
			"labels":    []map[string]string{{"name": "wip"}},
			"createdAt": "2026-05-01T00:00:00Z", "updatedAt": "2026-05-01T00:00:00Z",
			"url": "https://github.com/owner/repo/issues/7",
		}
		fake := &fakeGH{listOut: mustJSON([]map[string]any{dependent, blocker})}
		got, err := newGHAdapter(t, fake, mapping).ListCandidates(context.Background())
		if err != nil {
			t.Fatalf("ListCandidates: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("want 0 (issue #42 held by open #7), got %d: %+v", len(got), got)
		}
	})

	t.Run("dispatched when blocker gone (fail-open)", func(t *testing.T) {
		// #7 is not in the open set → treated as closed/satisfied.
		fake := &fakeGH{listOut: mustJSON([]map[string]any{dependent})}
		got, err := newGHAdapter(t, fake, mapping).ListCandidates(context.Background())
		if err != nil {
			t.Fatalf("ListCandidates: %v", err)
		}
		if len(got) != 1 || got[0].ID != "github:owner/repo#42" {
			t.Fatalf("want #42 dispatched, got %+v", got)
		}
	})
}

// A MISSING issue (deleted, transferred) must map Release onto
// ErrNotFound like the Forgejo twin: callers treat that absence as
// benign — without the mapping a deleted issue's claim-journal entry
// was retried and warned at every boot, for ever.
func TestGitHubReleaseMapsMissingIssueToNotFound(t *testing.T) {
	fake := &fakeGH{}
	a := newGHAdapter(t, fake, nil)
	fake.editErr = errors.New("GraphQL: Could not resolve to an issue or pull request with the number of 7. (repository.issue)")
	if err := a.Release(context.Background(), "github:owner/repo#7", "m"); !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("Release on a deleted issue = %v, want ErrNotFound — its journal entry is otherwise retried at every boot for ever", err)
	}
	// Any OTHER failure stays a real error (a 503 must keep the entry).
	fake.editErr = errors.New("HTTP 503: service unavailable")
	if err := a.Release(context.Background(), "github:owner/repo#7", "m"); err == nil || errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("a transient failure must stay a real error, got %v", err)
	}
}

// TestGitHubReleaseNarrowsRest404ToTheIssue: GitHub answers 404 (not 403)
// for a repository a token can no longer see, so a REST 404 that names
// only the REPO is a permission regression, not a gone issue — mapping it
// to ErrNotFound drops the claim-journal entry (this adapter's only
// recovery path) while the claim label stays on the issue. Only a 404
// whose URL names THIS issue is the permanent shape.
func TestGitHubReleaseNarrowsRest404ToTheIssue(t *testing.T) {
	fake := &fakeGH{}
	a := newGHAdapter(t, fake, nil)
	fake.editErr = errors.New("HTTP 404: Not Found (https://api.github.com/repos/owner/repo/issues/638/labels/iterion-claimed)")
	if err := a.Release(context.Background(), "github:owner/repo#638", "m"); !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("an issue-scoped REST 404 = %v, want ErrNotFound (the issue is gone; the entry would be retried at every boot)", err)
	}
	for _, msg := range []string{
		"HTTP 404: Not Found (https://api.github.com/repos/owner/repo)",
		"HTTP 404: Not Found (https://api.github.com/repos/owner/repo/issues/6380)",
		"GraphQL: Could not resolve to a Repository with the name 'owner/repo'. (repository)",
	} {
		fake.editErr = errors.New(msg)
		if err := a.Release(context.Background(), "github:owner/repo#638", "m"); err == nil || errors.Is(err, tracker.ErrNotFound) {
			t.Fatalf("%q must stay a real error (the journal entry is the only recovery path), got %v", msg, err)
		}
	}
}

// The claim LABEL deleted from the repo is the same permanent shape the
// missing-ISSUE mapping closed: gh refuses the remove, and a non-benign
// error kept the journal entry retried + warned at every boot for ever.
// Error text captured from gh 2.99.0 against a real repo.
func TestGitHubReleaseMapsDeletedClaimLabelToNotFound(t *testing.T) {
	fake := &fakeGH{}
	a := newGHAdapter(t, fake, nil)
	fake.editErr = errors.New("failed to update https://github.com/o/r/issues/638: 'iterion-claimed' not found\nfailed to update 1 issue")
	if err := a.Release(context.Background(), "github:owner/repo#638", "m"); !errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("Release with the claim label deleted from the repo = %v, want ErrNotFound", err)
	}
	// An UNRELATED label's not-found must NOT match (the anchor is the
	// configured claim label): that error is a real one to surface.
	fake.editErr = errors.New("failed to update https://github.com/o/r/issues/638: 'wontfix' not found")
	if err := a.Release(context.Background(), "github:owner/repo#638", "m"); err == nil || errors.Is(err, tracker.ErrNotFound) {
		t.Fatalf("an unrelated label's not-found mapped to ErrNotFound: %v", err)
	}
}
