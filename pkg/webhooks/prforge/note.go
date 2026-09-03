package prforge

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// EventHeaderIssueComment is the X-{GitHub,Forgejo,Gitea}-Event value for an
// issue/PR comment. Both forge families post PR comments under this event (a
// PR is an issue with a non-null pull_request link). The handler filters on
// this constant and the comment's leading slash-command.
const EventHeaderIssueComment = "issue_comment"

// IssueCommentEvent is the subset of the issue_comment webhook payload we
// decode. The shape is identical between GitHub and Forgejo/Gitea for the
// fields we read.
type IssueCommentEvent struct {
	Action     string     `json:"action"` // "created" | "edited" | "deleted"
	Repository Repository `json:"repository"`
	Issue      struct {
		Number      int64  `json:"number"`
		Title       string `json:"title"`
		Body        string `json:"body"`
		HTMLURL     string `json:"html_url"`
		State       string `json:"state"` // "open" | "closed"
		PullRequest *struct {
			HTMLURL string `json:"html_url"`
		} `json:"pull_request"` // non-null ⇒ the issue is a pull request
	} `json:"issue"`
	Comment struct {
		ID      int64  `json:"id"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
	} `json:"comment"`
	Sender Sender `json:"sender"`
}

// ParsedNote is the normalized issue/PR-comment view the inbound handler
// consumes — mirrors gitlab.ParsedNote in spirit (the comment to authorize +
// the repo + the slash-command).
type ParsedNote struct {
	RepoID        int64
	ProjectPath   string // "owner/repo"
	CloneURL      string
	IssueNumber   int64
	IsPullRequest bool   // the comment is on a PR (not a plain issue)
	IssueState    string // "open" | "closed"
	IssueTitle    string
	IssueBody     string
	IssueURL      string // the issue/PR's own web URL (html_url) — the comment subject
	PRURL         string // the PR's web URL, when IsPullRequest
	CommentID     int64
	CommentBody   string
	CommentURL    string
	AuthorLogin   string
	Action        string // comment action ("created" | …)
}

// ParseIssueComment decodes an issue_comment webhook body from GitHub or
// Forgejo/Gitea (one shared wire shape).
func ParseIssueComment(body []byte) (ParsedNote, error) {
	var e IssueCommentEvent
	if err := json.Unmarshal(body, &e); err != nil {
		return ParsedNote{}, fmt.Errorf("prforge: decode issue_comment event: %w", err)
	}
	p := ParsedNote{
		RepoID:      e.Repository.ID,
		ProjectPath: e.Repository.FullName,
		CloneURL:    e.Repository.CloneURL,
		IssueNumber: e.Issue.Number,
		IssueState:  e.Issue.State,
		IssueTitle:  e.Issue.Title,
		IssueBody:   e.Issue.Body,
		IssueURL:    e.Issue.HTMLURL,
		CommentID:   e.Comment.ID,
		CommentBody: e.Comment.Body,
		CommentURL:  e.Comment.HTMLURL,
		AuthorLogin: e.Sender.Login,
		Action:      e.Action,
	}
	if e.Issue.PullRequest != nil {
		p.IsPullRequest = true
		p.PRURL = e.Issue.PullRequest.HTMLURL
	}
	return p, nil
}

// Surface reports the comment surface for command-scope matching: "pr" when
// the comment is on a pull request, else "issue".
func (p ParsedNote) Surface() string {
	if p.IsPullRequest {
		return "pr"
	}
	return "issue"
}

// SubjectID is the stable per-comment id used in delivery records +
// idempotency (one launch per comment).
func (p ParsedNote) SubjectID() string {
	return "comment:" + strconv.FormatInt(p.CommentID, 10)
}

// ParentSubjectID names the PULL REQUEST a comment hangs off ("pr:7"), or
// "" when the comment sits on a plain issue. Distinct from SubjectID,
// which identifies the comment itself and is what per-comment idempotency
// keys on: a consumer asking "what did this pull request launch" — the
// closed-PR stop — cannot find a `/billy` run through "comment:99".
func (p ParsedNote) ParentSubjectID() string {
	if !p.IsPullRequest || p.IssueNumber == 0 {
		return ""
	}
	return "pr:" + strconv.FormatInt(p.IssueNumber, 10)
}

// Command extracts a leading slash-command from the comment body, e.g.
// "/featurly add export" → ("featurly", "add export"). Returns ("", "") when
// the comment does not start with a command. Delegates to
// webhooks.ParseSlashCommand so every comment surface shares one grammar
// (case-insensitive, tolerates leading blank / quote-reply lines).
func (p ParsedNote) Command() (cmd, args string) {
	return webhooks.ParseSlashCommand(p.CommentBody)
}
