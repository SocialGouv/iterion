package prforge

import (
	"encoding/json"
	"fmt"
	"strconv"
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
		Head    struct {
			SHA  string `json:"sha"`
			Ref  string `json:"ref"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
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
	// head.repo. Read by IsCrossRepo for the fork guard.
	HeadRepoFullName string
}

// IsCrossRepo reports whether the PR's head branch lives in a DIFFERENT repo
// than its base — i.e. the PR comes from a fork. Same semantics as
// Parsed.IsCrossRepo: on a fork, CloneURL (the BASE repo) and SourceBranch
// (a HEAD-repo ref) do not name the same repository, so a launch would check
// out a missing — or worse, a same-named base — branch. An empty head repo
// (minimal/legacy payloads) is treated as same-repo to avoid falsely gating
// a trusted internal PR.
func (p ParsedReviewComment) IsCrossRepo() bool {
	return p.HeadRepoFullName != "" && p.HeadRepoFullName != p.ProjectPath
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
	}
	if p.ThreadRootID == 0 {
		p.ThreadRootID = p.CommentID
	}
	if p.AuthorLogin == "" {
		p.AuthorLogin = e.Sender.Login
	}
	return p, nil
}

// SubjectID is the stable per-comment id used in delivery records +
// idempotency (one launch per reply).
func (p ParsedReviewComment) SubjectID() string {
	return "rc:" + strconv.FormatInt(p.CommentID, 10)
}

// PRSubjectID is the subject of the pull request whose review thread this
// reply belongs to — the same string Parsed.SubjectID() builds for the
// pull_request lane, so a run launched from a thread reply can be found when
// that PR later closes. Always a PR here: the event only exists on one.
func (p ParsedReviewComment) PRSubjectID() string {
	return "pr:" + strconv.FormatInt(p.PRNumber, 10)
}
