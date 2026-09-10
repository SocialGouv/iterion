package prforge

import "testing"

const reviewCommentReplyFixture = `{
  "action": "created",
  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
  "comment": {"id": 9002, "in_reply_to_id": 9001, "body": "why is this reachable?",
    "html_url": "https://github.com/acme/widgets/pull/7#discussion_r9002",
    "path": "pkg/x/y.go", "user": {"login": "alice"}},
  "pull_request": {"number": 7, "state": "open", "title": "Add X", "body": "desc",
    "html_url": "https://github.com/acme/widgets/pull/7",
    "head": {"sha": "abc123", "ref": "feature/x"}, "base": {"ref": "main"}},
  "sender": {"login": "alice"}
}`

func TestParseReviewComment(t *testing.T) {
	p, err := ParseReviewComment([]byte(reviewCommentReplyFixture))
	if err != nil {
		t.Fatal(err)
	}
	if p.ProjectPath != "acme/widgets" || p.PRNumber != 7 || p.PRState != "open" {
		t.Fatalf("PR context: %+v", p)
	}
	if p.CommentID != 9002 || p.ThreadRootID != 9001 {
		t.Fatalf("a reply's ThreadRootID must be in_reply_to_id: %+v", p)
	}
	if p.AuthorLogin != "alice" || p.CommentBody != "why is this reachable?" {
		t.Fatalf("comment fields: %+v", p)
	}
	if p.HeadSHA != "abc123" || p.SourceBranch != "feature/x" || p.TargetBranch != "main" {
		t.Fatalf("branch fields: %+v", p)
	}
	if p.SubjectID() != "rc:9002" {
		t.Fatalf("SubjectID: %q", p.SubjectID())
	}
}

func TestParseReviewCommentTopLevelIsItsOwnRoot(t *testing.T) {
	body := []byte(`{
  "action": "created",
  "repository": {"id": 42, "full_name": "acme/widgets"},
  "comment": {"id": 9001, "body": "inline note", "user": {"login": "revi-bot"}},
  "pull_request": {"number": 7, "state": "open"},
  "sender": {"login": "revi-bot"}
}`)
	p, err := ParseReviewComment(body)
	if err != nil {
		t.Fatal(err)
	}
	if p.ThreadRootID != 9001 {
		t.Fatalf("a thread-opening comment roots its own thread: %+v", p)
	}
}

func TestParseReviewCommentForkCarriesHeadRepo(t *testing.T) {
	body := []byte(`{
  "action": "created",
  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
  "comment": {"id": 9002, "in_reply_to_id": 9001, "body": "q", "user": {"login": "mallory"}},
  "pull_request": {"number": 7, "state": "open",
    "head": {"sha": "abc", "ref": "main", "repo": {"full_name": "mallory/widgets"}},
    "base": {"ref": "main"}},
  "sender": {"login": "mallory"}
}`)
	p, err := ParseReviewComment(body)
	if err != nil {
		t.Fatal(err)
	}
	if p.HeadRepoFullName != "mallory/widgets" || p.SameRepoAsBase() {
		t.Fatalf("fork head repo must be decoded and flagged: %+v", p)
	}
}

// A payload that does not name the head repo is NOT proven same-repo. The
// reply lane launches on `<base>.CloneURL + p.SourceBranch`, and an unnamed
// head is what a DELETED fork looks like — indistinguishable from a legacy
// payload, so the pair cannot be trusted to name one repository. Refusing
// costs a legacy sender its auto-reply; admitting answers a fork author
// with a review grounded in the base repo's code.
func TestParseReviewCommentUnnamedHeadRepoIsNotProvenSameRepo(t *testing.T) {
	p, err := ParseReviewComment([]byte(reviewCommentReplyFixture))
	if err != nil {
		t.Fatal(err)
	}
	if p.HeadRepoFullName != "" {
		t.Fatalf("this fixture must carry no head repo, got %q", p.HeadRepoFullName)
	}
	if p.SameRepoAsBase() {
		t.Fatalf("an unnamed head repo must never be proven same-repo: %+v", p)
	}
}
