package prforge

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// EventHeaderIssues is the X-{GitHub,Forgejo,Gitea}-Event value for an
// issue *lifecycle* event (opened, labeled, unlabeled, closed, …) — as
// opposed to issue_comment, which fires on a comment. A labeled delivery
// carries the single label just added under .label, which is what the
// "label X to launch a bot" flow keys on. The handler filters on this
// constant and the "labeled" action.
const EventHeaderIssues = "issues"

// IssuesEvent is the subset of the issues webhook payload we decode. The
// wire shape is shared between GitHub and Forgejo/Gitea for the fields we
// read; .label carries the single label added/removed on a
// labeled/unlabeled action (absent/empty on opened/closed).
type IssuesEvent struct {
	Action     string     `json:"action"` // "opened" | "labeled" | "unlabeled" | "closed" | …
	Repository Repository `json:"repository"`
	Issue      struct {
		Number  int64  `json:"number"`
		Title   string `json:"title"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
		State   string `json:"state"` // "open" | "closed"
		User    Sender `json:"user"`
		// AuthorAssociation is GitHub's relationship of the issue author to
		// the repo (OWNER|MEMBER|COLLABORATOR|CONTRIBUTOR|NONE|…); Forgejo
		// omits it (empty).
		AuthorAssociation string `json:"author_association"`
	} `json:"issue"`
	Label  Label  `json:"label"`
	Sender Sender `json:"sender"`
}

// Label is the single label carried on a labeled/unlabeled issues event.
type Label struct {
	Name string `json:"name"`
}

// ParsedIssue is the normalized issue-lifecycle view the inbound handler
// consumes — the repo to clone, the issue to implement + back-link, and
// the label that triggered the delivery.
type ParsedIssue struct {
	RepoID      int64
	ProjectPath string // "owner/repo"
	CloneURL    string
	IssueNumber int64
	Action      string // "labeled" | "unlabeled" | "opened" | …
	LabelName   string // the label added/removed on this event (labeled/unlabeled)
	IssueTitle  string
	IssueBody   string
	IssueURL    string // the issue's own web URL (html_url) — the back-link target
	IssueState  string // "open" | "closed"
	SenderLogin string
	// IssueAuthorLogin is the login that OPENED the issue (== SenderLogin on
	// an "opened" action; distinct on "labeled", where the sender is the
	// labeler). The author-trust gate classifies this identity.
	IssueAuthorLogin string
	// AuthorAssociation is GitHub's author↔repo relationship for the issue
	// author (empty on Forgejo) — the no-API-call trust fast path.
	AuthorAssociation string
}

// ParseIssues decodes an issues webhook body from GitHub or Forgejo/Gitea
// (one shared wire shape). We reject malformed bodies early so the handler
// returns a clean 400 instead of crashing on a nil deref.
func ParseIssues(body []byte) (ParsedIssue, error) {
	var e IssuesEvent
	if err := json.Unmarshal(body, &e); err != nil {
		return ParsedIssue{}, fmt.Errorf("prforge: decode issues event: %w", err)
	}
	return ParsedIssue{
		RepoID:            e.Repository.ID,
		ProjectPath:       e.Repository.FullName,
		CloneURL:          e.Repository.CloneURL,
		IssueNumber:       e.Issue.Number,
		Action:            e.Action,
		LabelName:         e.Label.Name,
		IssueTitle:        e.Issue.Title,
		IssueBody:         e.Issue.Body,
		IssueURL:          e.Issue.HTMLURL,
		IssueState:        e.Issue.State,
		SenderLogin:       e.Sender.Login,
		IssueAuthorLogin:  e.Issue.User.Login,
		AuthorAssociation: e.Issue.AuthorAssociation,
	}, nil
}

// IsLabeled reports whether this is a "labeled" action carrying a
// non-empty label name — the only issues action that auto-triggers a
// launch. Other actions (opened/closed/unlabeled/assigned/…) are filtered;
// re-labeling is the explicit operator gesture the flow keys on.
func (p ParsedIssue) IsLabeled() bool {
	return p.Action == "labeled" && p.LabelName != ""
}

// IsOpened reports whether this is the "opened" action — a freshly created
// issue. The zero-touch path (webhook AutoImplementOnOpen) keys on it to
// dispatch the implementer without waiting for a label.
func (p ParsedIssue) IsOpened() bool {
	return p.Action == "opened"
}

// SubjectID is the stable per-issue identifier used in delivery records.
func (p ParsedIssue) SubjectID() string {
	return "issue:" + strconv.FormatInt(p.IssueNumber, 10)
}
