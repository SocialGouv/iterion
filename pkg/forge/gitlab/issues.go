package gitlab

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// gitlabUser is the lowest-common-denominator user shape GitLab embeds in
// issues/merge-requests (author, assignees). Only the username is consumed.
type gitlabUser struct {
	Username string `json:"username"`
	Name     string `json:"name"`
}

// gitlabIssue is the GitLab issue shape (read). GitLab addresses an issue by
// its per-project `iid` (not the global `id`); `state` is "opened"/"closed",
// `description` is the body, `web_url` is the URL.
type gitlabIssue struct {
	IID         int          `json:"iid"`
	Title       string       `json:"title"`
	Description string       `json:"description"`
	State       string       `json:"state"`
	WebURL      string       `json:"web_url"`
	Labels      []string     `json:"labels"`
	Author      gitlabUser   `json:"author"`
	Assignees   []gitlabUser `json:"assignees"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

// toRef normalizes a GitLab issue onto forge.IssueRef. The per-project iid is
// the number the rest of pkg/forge addresses; state "opened"→"open".
func (gi gitlabIssue) toRef() forge.IssueRef {
	assignees := make([]string, 0, len(gi.Assignees))
	for _, a := range gi.Assignees {
		if a.Username != "" {
			assignees = append(assignees, a.Username)
		}
	}
	return forge.IssueRef{
		Number:    gi.IID,
		Title:     gi.Title,
		Body:      gi.Description,
		State:     normIssueState(gi.State),
		URL:       gi.WebURL,
		Labels:    gi.Labels,
		Assignees: assignees,
		Author:    gi.Author.Username,
		CreatedAt: gi.CreatedAt,
		UpdatedAt: gi.UpdatedAt,
	}
}

// normIssueState maps GitLab's "opened"/"closed" onto the forge "open"/"closed"
// vocabulary. Any other value passes through unchanged.
func normIssueState(s string) string {
	if s == "opened" {
		return "open"
	}
	return s
}

// issueStateQuery maps a forge IssueListOptions.State onto GitLab's `state`
// query value ("opened"/"closed"); "all" / empty → no filter (GitLab lists all).
func issueStateQuery(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "open", "opened":
		return "opened"
	case "closed":
		return "closed"
	default:
		return ""
	}
}

// ListIssues lists a project's issues, normalized to forge.IssueRef. Powers
// forge→board one-way sync (incremental via opts.Since → updated_after).
func (c *AdminClient) ListIssues(ctx context.Context, repo string, opts forge.IssueListOptions) ([]forge.IssueRef, error) {
	vals := url.Values{}
	if st := issueStateQuery(opts.State); st != "" {
		vals.Set("state", st)
	}
	if len(opts.Labels) > 0 {
		vals.Set("labels", strings.Join(opts.Labels, ","))
	}
	if !opts.Since.IsZero() {
		vals.Set("updated_after", opts.Since.UTC().Format(time.RFC3339))
	}
	page := opts.Page
	if page <= 0 {
		page = 1
	}
	vals.Set("page", strconv.Itoa(page))
	if opts.PerPage > 0 {
		vals.Set("per_page", strconv.Itoa(opts.PerPage))
	}

	var issues []gitlabIssue
	code, err := c.do(ctx, http.MethodGet, "/projects/"+projectID(repo)+"/issues?"+vals.Encode(), nil, &issues)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, statusErr("list issues", code)
	}
	out := make([]forge.IssueRef, 0, len(issues))
	for _, gi := range issues {
		out = append(out, gi.toRef())
	}
	return out, nil
}

// GetIssue fetches one issue by its per-project iid.
func (c *AdminClient) GetIssue(ctx context.Context, repo string, number int) (forge.IssueRef, error) {
	var gi gitlabIssue
	code, err := c.do(ctx, http.MethodGet, "/projects/"+projectID(repo)+"/issues/"+strconv.Itoa(number), nil, &gi)
	if err != nil {
		return forge.IssueRef{}, err
	}
	if code != http.StatusOK {
		return forge.IssueRef{}, statusErr("get issue", code)
	}
	return gi.toRef(), nil
}

// CreateIssue creates an issue (board→forge push). Labels are sent as a
// comma-joined string (GitLab's write shape); assignees would need numeric
// ids, which the board doesn't carry, so they're left to a later mapping.
func (c *AdminClient) CreateIssue(ctx context.Context, repo string, in forge.NewIssue) (forge.IssueRef, error) {
	body := map[string]any{"title": in.Title}
	if in.Body != "" {
		body["description"] = in.Body
	}
	if len(in.Labels) > 0 {
		body["labels"] = strings.Join(in.Labels, ",")
	}
	var gi gitlabIssue
	code, err := c.do(ctx, http.MethodPost, "/projects/"+projectID(repo)+"/issues", body, &gi)
	if err != nil {
		return forge.IssueRef{}, err
	}
	if code/100 != 2 {
		return forge.IssueRef{}, statusErr("create issue", code)
	}
	return gi.toRef(), nil
}

// UpdateIssue applies a partial patch to an issue. State transitions map onto
// GitLab's `state_event` (close|reopen); nil patch fields are left untouched.
func (c *AdminClient) UpdateIssue(ctx context.Context, repo string, number int, patch forge.IssuePatch) (forge.IssueRef, error) {
	body := map[string]any{}
	if patch.Title != nil {
		body["title"] = *patch.Title
	}
	if patch.Body != nil {
		body["description"] = *patch.Body
	}
	if patch.Labels != nil {
		body["labels"] = strings.Join(*patch.Labels, ",")
	}
	if patch.State != nil {
		switch strings.ToLower(strings.TrimSpace(*patch.State)) {
		case "closed":
			body["state_event"] = "close"
		case "open", "opened":
			body["state_event"] = "reopen"
		}
	}
	var gi gitlabIssue
	code, err := c.do(ctx, http.MethodPut, "/projects/"+projectID(repo)+"/issues/"+strconv.Itoa(number), body, &gi)
	if err != nil {
		return forge.IssueRef{}, err
	}
	if code/100 != 2 {
		return forge.IssueRef{}, statusErr("update issue", code)
	}
	return gi.toRef(), nil
}

// gitlabNote is the slice of GitLab's issue-note object we normalize. Notes
// carry no per-note web URL in the create response, so CommentRef.URL is left
// empty for GitLab.
type gitlabNote struct {
	ID        int64      `json:"id"`
	Body      string     `json:"body"`
	Author    gitlabUser `json:"author"`
	CreatedAt time.Time  `json:"created_at"`
}

// CommentIssue posts a note on an issue.
func (c *AdminClient) CommentIssue(ctx context.Context, repo string, number int, body string) (forge.CommentRef, error) {
	var n gitlabNote
	code, err := c.do(ctx, http.MethodPost, "/projects/"+projectID(repo)+"/issues/"+strconv.Itoa(number)+"/notes", map[string]any{"body": body}, &n)
	if err != nil {
		return forge.CommentRef{}, err
	}
	if code/100 != 2 {
		return forge.CommentRef{}, statusErr("create note", code)
	}
	return forge.CommentRef{
		ID:        strconv.FormatInt(n.ID, 10),
		Body:      n.Body,
		Author:    n.Author.Username,
		CreatedAt: n.CreatedAt,
	}, nil
}

// CommentPullRequest posts a note on a MERGE REQUEST — the counterpart to
// CommentIssue for a caller that knows its subject is an MR, not a bare
// issue. Unlike GitHub/Forgejo, whose issues endpoint already serves PRs,
// GitLab addresses merge requests as a resource SEPARATE from issues (an MR
// and an issue can share the same iid in one project), so CommentIssue
// would land on — or 404 against — the wrong resource. Satisfies the
// package-server-local gitlabPullCommenter capability the /revi approve
// reply lane resolves.
func (c *AdminClient) CommentPullRequest(ctx context.Context, repo string, number int, body string) (forge.CommentRef, error) {
	var n gitlabNote
	code, err := c.do(ctx, http.MethodPost, "/projects/"+projectID(repo)+"/merge_requests/"+strconv.Itoa(number)+"/notes", map[string]any{"body": body}, &n)
	if err != nil {
		return forge.CommentRef{}, err
	}
	if code/100 != 2 {
		return forge.CommentRef{}, statusErr("create mr note", code)
	}
	return forge.CommentRef{
		ID:        strconv.FormatInt(n.ID, 10),
		Body:      n.Body,
		Author:    n.Author.Username,
		CreatedAt: n.CreatedAt,
	}, nil
}

var _ forge.IssueClient = (*AdminClient)(nil)
