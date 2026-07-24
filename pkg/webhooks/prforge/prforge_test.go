package prforge

import "testing"

// githubOpenPR is the wire shape GitHub sends — fields ordered with
// repository before pull_request, html_url at top level on repository.
const githubOpenPR = `{
  "action": "opened",
  "number": 7,
  "repository": {
    "id": 42,
    "full_name": "acme/widgets",
    "clone_url": "https://github.com/acme/widgets.git",
    "html_url": "https://github.com/acme/widgets"
  },
  "pull_request": {
    "number": 7,
    "title": "Add X",
    "body": "Implements X",
    "html_url": "https://github.com/acme/widgets/pull/7",
    "state": "open",
    "head": {"ref": "feature/x", "sha": "abc123"},
    "base": {"ref": "main"}
  },
  "sender": {"login": "alice"}
}`

// sameRepoPR carries head.repo == base repo (an internal-branch PR): NOT a fork.
const sameRepoPR = `{
  "action": "opened",
  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
  "pull_request": {
    "number": 7, "title": "Add X", "body": "Fixes #12", "state": "open",
    "head": {"ref": "feature/x", "sha": "abc123", "repo": {"full_name": "acme/widgets"}},
    "base": {"ref": "main", "repo": {"full_name": "acme/widgets"}}
  },
  "sender": {"login": "alice"}
}`

// forkPR carries head.repo in a DIFFERENT owner — the fork-guard signal.
const forkPR = `{
  "action": "opened",
  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
  "pull_request": {
    "number": 8, "title": "Add Y", "body": "Fixes #13", "state": "open",
    "head": {"ref": "patch-1", "sha": "def456", "repo": {"full_name": "mallory/widgets"}},
    "base": {"ref": "main", "repo": {"full_name": "acme/widgets"}}
  },
  "sender": {"login": "mallory"}
}`

// forgejoOpenPR is the wire shape Forgejo/Gitea sends — pull_request
// before repository (the only structural difference from GitHub), and
// the codeberg.org URL flavour.
const forgejoOpenPR = `{
  "action": "opened",
  "number": 7,
  "pull_request": {
    "number": 7,
    "title": "Add X",
    "body": "Implements X",
    "html_url": "https://codeberg.org/acme/widgets/pulls/7",
    "state": "open",
    "head": {"ref": "feature/x", "sha": "abc123"},
    "base": {"ref": "main"}
  },
  "repository": {
    "id": 42,
    "full_name": "acme/widgets",
    "clone_url": "https://codeberg.org/acme/widgets.git"
  },
  "sender": {"login": "alice"}
}`

func TestParsePullRequest_GitHub(t *testing.T) {
	p, err := ParsePullRequest([]byte(githubOpenPR))
	if err != nil {
		t.Fatal(err)
	}
	if p.RepoID != 42 || p.ProjectPath != "acme/widgets" || p.CloneURL != "https://github.com/acme/widgets.git" {
		t.Fatalf("repo: %+v", p)
	}
	if p.PRNumber != 7 || p.SourceBranch != "feature/x" || p.TargetBranch != "main" || p.HeadSHA != "abc123" {
		t.Fatalf("pr: %+v", p)
	}
	if p.PRURL != "https://github.com/acme/widgets/pull/7" || p.SubjectID() != "pr:7" {
		t.Fatalf("url/subject: %+v", p)
	}
	if p.SenderLogin != "alice" {
		t.Fatalf("sender: %+v", p)
	}
	if !p.IsReviewable() {
		t.Fatal("opened PR should be reviewable")
	}
}

func TestParsePullRequest_Forgejo(t *testing.T) {
	p, err := ParsePullRequest([]byte(forgejoOpenPR))
	if err != nil {
		t.Fatal(err)
	}
	if p.RepoID != 42 || p.ProjectPath != "acme/widgets" || p.CloneURL != "https://codeberg.org/acme/widgets.git" {
		t.Fatalf("repo: %+v", p)
	}
	if p.PRNumber != 7 || p.SourceBranch != "feature/x" || p.TargetBranch != "main" || p.HeadSHA != "abc123" {
		t.Fatalf("pr: %+v", p)
	}
	if p.SubjectID() != "pr:7" || p.SenderLogin != "alice" {
		t.Fatalf("subject/sender: %+v", p)
	}
	if !p.IsReviewable() {
		t.Fatal("opened should be reviewable")
	}
}

// TestIsCrossRepo guards the fork-guard signal: a PR whose head branch lives
// in a different repo than its base is a fork (untrusted), and a payload with
// no head.repo defaults to same-repo so a trusted internal PR is never falsely
// gated off the auto-launch path.
func TestIsCrossRepo(t *testing.T) {
	same, err := ParsePullRequest([]byte(sameRepoPR))
	if err != nil {
		t.Fatal(err)
	}
	if same.HeadRepoFullName != "acme/widgets" {
		t.Fatalf("same-repo head: %q", same.HeadRepoFullName)
	}
	if same.IsCrossRepo() {
		t.Error("same-repo PR must NOT be cross-repo")
	}

	fork, err := ParsePullRequest([]byte(forkPR))
	if err != nil {
		t.Fatal(err)
	}
	if fork.HeadRepoFullName != "mallory/widgets" {
		t.Fatalf("fork head: %q", fork.HeadRepoFullName)
	}
	if !fork.IsCrossRepo() {
		t.Error("fork PR MUST be cross-repo (fork-guard signal)")
	}

	// Legacy/minimal payload with no head.repo → same-repo (not a fork).
	min, err := ParsePullRequest([]byte(githubOpenPR))
	if err != nil {
		t.Fatal(err)
	}
	if min.IsCrossRepo() {
		t.Error("PR with no head.repo must default to same-repo")
	}
}

func TestParsePullRequest_MalformedFails(t *testing.T) {
	if _, err := ParsePullRequest([]byte(`{bad`)); err == nil {
		t.Fatal("malformed json should error")
	}
}

func TestIsReviewable(t *testing.T) {
	cases := []struct {
		action string
		draft  bool
		want   bool
	}{
		{"opened", false, true},
		{"reopened", false, true},
		// A draft PR being marked ready-for-review is THE auto-trigger.
		{"ready_for_review", false, true},
		// A DRAFT PR never auto-triggers, whatever the action — the author
		// is still iterating; the trigger is ready_for_review (draft=false).
		{"opened", true, false},
		{"reopened", true, false},
		// GitHub spells the push action "synchronize"; Gitea spells it
		// "synchronized". Both must filter, since re-review is on-demand.
		{"synchronize", false, false},
		{"synchronized", false, false},
		{"edited", false, false},
		{"labeled", false, false},
		{"closed", false, false},
		{"review_requested", false, false},
	}
	for _, c := range cases {
		p := Parsed{Action: c.action, Draft: c.draft}
		if got := p.IsReviewable(); got != c.want {
			t.Errorf("action=%q draft=%v => %v want %v", c.action, c.draft, got, c.want)
		}
	}
}

func TestIsSynchronize(t *testing.T) {
	cases := []struct {
		action string
		draft  bool
		want   bool
	}{
		{"synchronize", false, true},  // GitHub push-to-PR
		{"synchronized", false, true}, // Gitea/Forgejo push-to-PR
		{"synchronize", true, false},  // a DRAFT push never re-reviews
		{"synchronized", true, false}, // idem Gitea/Forgejo
		{"opened", false, false},
		{"reopened", false, false},
		{"edited", false, false},
	}
	for _, c := range cases {
		if got := (Parsed{Action: c.action, Draft: c.draft}).IsSynchronize(); got != c.want {
			t.Errorf("action=%q draft=%v => %v want %v", c.action, c.draft, got, c.want)
		}
	}
}

// Allowlist matching tests live in pkg/webhooks/match_test.go (the
// canonical webhooks.MatchEvent + MatchProject are exercised there with
// every provider's default kinds).
