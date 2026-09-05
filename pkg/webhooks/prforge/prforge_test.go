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

// TestSameRepoAsBase guards the fork guard, which is fail-CLOSED: a lane
// launching on `<base>.CloneURL + head branch` may only proceed when the
// head is PROVEN to live in the base repo.
func TestSameRepoAsBase(t *testing.T) {
	same, err := ParsePullRequest([]byte(sameRepoPR))
	if err != nil {
		t.Fatal(err)
	}
	if same.HeadRepoFullName != "acme/widgets" {
		t.Fatalf("same-repo head: %q", same.HeadRepoFullName)
	}
	if !same.SameRepoAsBase() {
		t.Error("a same-repo PR must be proven same-repo")
	}

	fork, err := ParsePullRequest([]byte(forkPR))
	if err != nil {
		t.Fatal(err)
	}
	if fork.HeadRepoFullName != "mallory/widgets" {
		t.Fatalf("fork head: %q", fork.HeadRepoFullName)
	}
	if fork.SameRepoAsBase() {
		t.Error("a fork PR must NOT be proven same-repo")
	}

	// The hole this predicate closes: `head.repo: null` is what a fork
	// looks like once it is DELETED, and it is indistinguishable from a
	// payload that never carried the field. Reading either as same-repo
	// aims the bot at the base repo with a head branch name the fork
	// author chose — a fixer would push LLM commits onto the base repo's
	// branch of that name.
	unnamed, err := ParsePullRequest([]byte(githubOpenPR))
	if err != nil {
		t.Fatal(err)
	}
	if unnamed.HeadRepoFullName != "" {
		t.Fatalf("this fixture must carry no head repo, got %q", unnamed.HeadRepoFullName)
	}
	if unnamed.SameRepoAsBase() {
		t.Error("an unnamed head repo must never be proven same-repo — a deleted fork has exactly this shape")
	}
}

// The GitHub/Forgejo "Request review" / "Re-request review" gesture arrives
// as a `review_requested` action carrying the targeted user. It is a manual
// gesture, so unlike the auto-review actions a draft does not suppress it.
func TestParsePullRequest_ReviewRequested(t *testing.T) {
	payload := `{
	  "action": "review_requested",
	  "sender": {"login": "alice"},
	  "requested_reviewer": {"login": "iterion-bot"},
	  "repository": {"id": 1, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
	  "pull_request": {"number": 5, "title": "t", "html_url": "https://github.com/acme/widgets/pull/5",
	    "state": "open", "draft": true, "updated_at": "2026-09-01T10:00:00Z",
	    "head": {"ref": "feat", "sha": "abc"}, "base": {"ref": "main"}}
	}`
	p, err := ParsePullRequest([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	if p.RequestedReviewerLogin != "iterion-bot" || p.UpdatedAt != "2026-09-01T10:00:00Z" {
		t.Fatalf("parsed: %+v", p)
	}
	if !p.ReviewRequestedFrom("iterion-bot") || !p.ReviewRequestedFrom("ITERION-BOT") {
		t.Fatal("ReviewRequestedFrom must match the targeted reviewer (case-insensitively), draft included")
	}
	if p.ReviewRequestedFrom("alice") || p.ReviewRequestedFrom("") {
		t.Fatal("only the targeted reviewer matches")
	}
	if p.IsReviewable() {
		t.Fatal("review_requested is not an auto-review action")
	}

	// A team review request carries no requested_reviewer — never matches.
	team := `{"action": "review_requested", "repository": {"full_name": "acme/widgets"},
	  "pull_request": {"number": 5, "head": {"ref": "feat", "sha": "abc"}, "base": {"ref": "main"}}}`
	p2, err := ParsePullRequest([]byte(team))
	if err != nil {
		t.Fatal(err)
	}
	if p2.ReviewRequestedFrom("iterion-bot") {
		t.Fatal("team review request must not match a user login")
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

func TestParsePullRequest_Labels(t *testing.T) {
	body := `{"action":"opened","number":5,"repository":{"full_name":"o/r"},"pull_request":{"number":5,"labels":[{"name":"ready"},{"name":"iterion:hold"},{"name":""}],"head":{"ref":"f","sha":"s"},"base":{"ref":"main"}}}`
	p, err := ParsePullRequest([]byte(body))
	if err != nil {
		t.Fatalf("ParsePullRequest: %v", err)
	}
	if len(p.Labels) != 2 || p.Labels[0] != "ready" || p.Labels[1] != "iterion:hold" {
		t.Fatalf("labels = %v, want [ready iterion:hold] (empty dropped)", p.Labels)
	}
}
