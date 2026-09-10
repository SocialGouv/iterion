package prforge

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// EventHeaderReviewComment is the X-GitHub-Event value for a comment in a
// PR review thread (an inline comment on the diff, or a reply inside one of
// those threads). Distinct from issue_comment: a review-thread reply never
// arrives as issue_comment, so the conversational reply-to-a-suggestion lane
// needs this event subscribed on the hook and handled here.
const EventHeaderReviewComment = "pull_request_review_comment"

// ReviewCommentEvent is the subset of the pull_request_review_comment
// webhook payload we decode (GitHub wire shape).
type ReviewCommentEvent struct {
	Action     string     `json:"action"` // "created" | "edited" | "deleted"
	Repository Repository `json:"repository"`
	Comment    struct {
		ID        int64  `json:"id"`
		InReplyTo int64  `json:"in_reply_to_id"` // 0 ⇒ this comment starts its thread
		Body      string `json:"body"`
		HTMLURL   string `json:"html_url"`
		Path      string `json:"path"`
		User      Sender `json:"user"`
	} `json:"comment"`
	PullRequest struct {
		Number  int64  `json:"number"`
		State   string `json:"state"` // "open" | "closed"
		Title   string `json:"title"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
		Head    Ref    `json:"head"`
		Base    struct {
			Ref string `json:"ref"`
		} `json:"base"`
	} `json:"pull_request"`
	Sender Sender `json:"sender"`
}

// ParsedReviewComment is the normalized review-thread-comment view the
// inbound handler consumes — the GitHub twin of gitlab.ParsedNote's
// reply-in-thread half. Unlike issue_comment, the payload carries the full
// PR head/base, so no follow-up API resolution is needed to launch.
type ParsedReviewComment struct {
	RepoID       int64
	ProjectPath  string // "owner/repo"
	CloneURL     string
	PRNumber     int64
	PRState      string // "open" | "closed"
	PRTitle      string
	PRBody       string
	PRURL        string
	HeadSHA      string
	SourceBranch string // PR head ref
	TargetBranch string // PR base ref
	CommentID    int64
	// ThreadRootID is the top-level comment id of the thread this comment
	// belongs to: in_reply_to_id when the comment is a reply, else the
	// comment's own id. It is what GitHub's reply endpoint
	// (/pulls/{n}/comments/{id}/replies) wants as {id}, so it is handed to
	// the bot as discussion_id.
	ThreadRootID int64
	CommentBody  string
	CommentURL   string
	CommentPath  string // file the thread anchors to
	AuthorLogin  string
	Action       string
	// HeadRepoFullName is the "owner/repo" the PR's head branch lives in —
	// differs from ProjectPath on a fork PR; empty when the payload omits
	// head.repo OR the head repo was deleted/blocked. Read by
	// SameRepoAsBase, which treats empty as NOT proven same-repo.
	HeadRepoFullName string
	// HeadRepoDeclared — see Parsed.HeadRepoDeclared.
	HeadRepoDeclared bool
}

// SameRepoAsBase reports whether the head branch is PROVEN to live in the
// base repo. Same contract, and same reason, as Parsed.SameRepoAsBase: the
// reply lane launches on `<base>.CloneURL + p.SourceBranch` too.
func (p ParsedReviewComment) SameRepoAsBase() bool {
	return forge.SameRepo(p.HeadRepoFullName, p.ProjectPath)
}

// HeadRepoWithheld — see Parsed.HeadRepoWithheld.
func (p ParsedReviewComment) HeadRepoWithheld() bool {
	return p.HeadRepoDeclared && p.HeadRepoFullName == ""
}

// ParseReviewComment decodes a pull_request_review_comment webhook body
// (GitHub wire shape).
func ParseReviewComment(body []byte) (ParsedReviewComment, error) {
	var e ReviewCommentEvent
	if err := json.Unmarshal(body, &e); err != nil {
		return ParsedReviewComment{}, fmt.Errorf("prforge: decode pull_request_review_comment event: %w", err)
	}
	p := ParsedReviewComment{
		RepoID:       e.Repository.ID,
		ProjectPath:  e.Repository.FullName,
		CloneURL:     e.Repository.CloneURL,
		PRNumber:     e.PullRequest.Number,
		PRState:      e.PullRequest.State,
		PRTitle:      e.PullRequest.Title,
		PRBody:       e.PullRequest.Body,
		PRURL:        e.PullRequest.HTMLURL,
		HeadSHA:      e.PullRequest.Head.SHA,
		SourceBranch: e.PullRequest.Head.Ref,
		TargetBranch: e.PullRequest.Base.Ref,
		CommentID:    e.Comment.ID,
		ThreadRootID: e.Comment.InReplyTo,
		CommentBody:  e.Comment.Body,
		CommentURL:   e.Comment.HTMLURL,
		CommentPath:  e.Comment.Path,
		AuthorLogin:  e.Comment.User.Login,
		Action:       e.Action,

		HeadRepoFullName: e.PullRequest.Head.Repo.FullName,
		HeadRepoDeclared: e.PullRequest.Head.RepoDeclared,
	}
	if p.ThreadRootID == 0 {
		p.ThreadRootID = p.CommentID
	}
	if p.AuthorLogin == "" {
		p.AuthorLogin = e.Sender.Login
	}
	return p, nil
}

// ParentSubjectID names the PULL REQUEST this review-thread comment hangs
// off ("pr:7"). See ParsedNote.ParentSubjectID for why the parent link is
// stored beside the comment's own subject id.
func (p ParsedReviewComment) ParentSubjectID() string {
	return "pr:" + strconv.FormatInt(p.PRNumber, 10)
}

// SubjectID is the stable per-comment id used in delivery records +
// idempotency (one launch per reply).
func (p ParsedReviewComment) SubjectID() string {
	return "rc:" + strconv.FormatInt(p.CommentID, 10)
}
