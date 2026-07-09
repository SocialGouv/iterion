package tracker_test

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// TestGitHubHasLinkedPR: the probe reports true only when an OPEN PR references
// the issue with a "#N" link (the same signal the board projection uses).
func TestGitHubHasLinkedPR(t *testing.T) {
	// PR #9 closes #12; PR #11 links nothing.
	const prList = `[{"number":9,"title":"Add subtract","body":"Implements subtraction.\n\nFixes #12"},{"number":11,"title":"Docs tidy","body":"no linked issue"}]`
	a, err := tracker.NewGitHub(tracker.GitHubOptions{
		Repo: "acme/widgets",
		Command: func(_ context.Context, args []string, _ []string) ([]byte, error) {
			if len(args) < 2 || args[0] != "pr" || args[1] != "list" {
				t.Fatalf("expected `gh pr list`, got %v", args)
			}
			return []byte(prList), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	has, err := a.HasLinkedPR(context.Background(), "github:acme/widgets#12")
	if err != nil || !has {
		t.Fatalf("issue #12 IS linked by PR #9: has=%v err=%v", has, err)
	}
	has, err = a.HasLinkedPR(context.Background(), "github:acme/widgets#99")
	if err != nil || has {
		t.Fatalf("issue #99 has no linked PR: has=%v err=%v", has, err)
	}
}

// TestGitHubListCandidates_AuthorAllowlist: with a non-empty AuthorAllowlist,
// only issues opened by an allowed author (case-insensitive) are candidates —
// the trusted-author scope for auto-dispatch.
func TestGitHubListCandidates_AuthorAllowlist(t *testing.T) {
	fake := &fakeGH{
		listOut: mustJSON([]map[string]any{
			{
				"number": 1, "title": "from alice", "state": "open",
				"labels":    []map[string]string{{"name": "ready"}},
				"author":    map[string]string{"login": "alice"},
				"createdAt": "2026-05-01T00:00:00Z", "updatedAt": "2026-05-01T00:00:00Z",
				"url": "https://github.com/owner/repo/issues/1",
			},
			{
				"number": 2, "title": "from mallory", "state": "open",
				"labels":    []map[string]string{{"name": "ready"}},
				"author":    map[string]string{"login": "mallory"},
				"createdAt": "2026-05-02T00:00:00Z", "updatedAt": "2026-05-02T00:00:00Z",
				"url": "https://github.com/owner/repo/issues/2",
			},
		}),
	}
	a, err := tracker.NewGitHub(tracker.GitHubOptions{
		Repo:            "owner/repo",
		Command:         fake.cmd,
		AuthorAllowlist: []string{"Alice"}, // case-insensitive
		StateMapping:    map[string]tracker.LabelSelector{"ready": {LabelsInclude: []string{"ready"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.ListCandidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.HasSuffix(got[0].ID, "#1") {
		t.Fatalf("only alice's issue #1 should be a candidate, got %+v", got)
	}
}

// TestGitHubApplyLabel: the visible bot:* association reuses the gh
// issue-edit --add-label seam.
func TestGitHubApplyLabel(t *testing.T) {
	var got []string
	a, err := tracker.NewGitHub(tracker.GitHubOptions{
		Repo: "acme/widgets",
		Command: func(_ context.Context, args []string, _ []string) ([]byte, error) {
			got = args
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.ApplyLabel(context.Background(), "github:acme/widgets#7", "bot:featurly"); err != nil {
		t.Fatal(err)
	}
	j := strings.Join(got, " ")
	if !strings.Contains(j, "issue edit 7") || !strings.Contains(j, "--repo acme/widgets") || !strings.Contains(j, "--add-label bot:featurly") {
		t.Fatalf("unexpected gh args: %v", got)
	}
}
