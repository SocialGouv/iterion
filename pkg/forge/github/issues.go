package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// IssueClient capability — forge→board sync (ListIssues) + board→forge push
// (Create/UpdateIssue). The GitHub issues endpoint conflates issues and pull
// requests (a PR is an issue with a non-null `pull_request` field), so the
// wire shape carries that field and IsPullRequest is set from it; the board
// sync filters PRs out.
var _ forge.IssueClient = (*AdminClient)(nil)

// githubIssue is the slice of the GitHub issue object we map to IssueRef.
// `pull_request` is present (non-null) only when the item is actually a PR.
type githubIssue struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
	Labels  []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Assignees []struct {
		Login string `json:"login"`
	} `json:"assignees"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	CreatedAt   time.Time        `json:"created_at"`
	UpdatedAt   time.Time        `json:"updated_at"`
	PullRequest *json.RawMessage `json:"pull_request"`
}

func (gi githubIssue) toRef() forge.IssueRef {
	labels := make([]string, 0, len(gi.Labels))
	for _, l := range gi.Labels {
		if l.Name != "" {
			labels = append(labels, l.Name)
		}
	}
	assignees := make([]string, 0, len(gi.Assignees))
	for _, a := range gi.Assignees {
		if a.Login != "" {
			assignees = append(assignees, a.Login)
		}
	}
	return forge.IssueRef{
		Number:        gi.Number,
		Title:         gi.Title,
		Body:          gi.Body,
		State:         gi.State,
		URL:           gi.HTMLURL,
		Labels:        labels,
		Assignees:     assignees,
		Author:        gi.User.Login,
		CreatedAt:     gi.CreatedAt,
		UpdatedAt:     gi.UpdatedAt,
		IsPullRequest: gi.PullRequest != nil,
	}
}

// ListIssues lists issues for repo ("owner/repo"). PRs are returned by the
// same endpoint but flagged via IsPullRequest so callers can drop them.
func (c *AdminClient) ListIssues(ctx context.Context, repo string, opts forge.IssueListOptions) ([]forge.IssueRef, error) {
	vals := url.Values{}
	state := opts.State
	if state == "" {
		state = "open"
	}
	vals.Set("state", state)
	if len(opts.Labels) > 0 {
		vals.Set("labels", strings.Join(opts.Labels, ","))
	}
	if !opts.Since.IsZero() {
		vals.Set("since", opts.Since.UTC().Format(time.RFC3339))
	}
	perPage := opts.PerPage
	if perPage <= 0 || perPage > 100 {
		perPage = 50
	}
	vals.Set("per_page", strconv.Itoa(perPage))
	page := opts.Page
	if page <= 0 {
		page = 1
	}
	vals.Set("page", strconv.Itoa(page))

	var raw []githubIssue
	code, err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/issues?"+vals.Encode(), nil, &raw)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, statusErr("GET issues", code)
	}
	out := make([]forge.IssueRef, 0, len(raw))
	for _, gi := range raw {
		out = append(out, gi.toRef())
	}
	return out, nil
}

// GetIssue fetches one issue (or PR) by number.
func (c *AdminClient) GetIssue(ctx context.Context, repo string, number int) (forge.IssueRef, error) {
	var gi githubIssue
	code, err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/issues/"+strconv.Itoa(number), nil, &gi)
	if err != nil {
		return forge.IssueRef{}, err
	}
	if code != http.StatusOK {
		return forge.IssueRef{}, statusErr("GET issue", code)
	}
	return gi.toRef(), nil
}

// CreateIssue opens a new issue (board→forge push).
func (c *AdminClient) CreateIssue(ctx context.Context, repo string, in forge.NewIssue) (forge.IssueRef, error) {
	body := map[string]any{"title": in.Title}
	if in.Body != "" {
		body["body"] = in.Body
	}
	if len(in.Labels) > 0 {
		body["labels"] = in.Labels
	}
	if len(in.Assignees) > 0 {
		body["assignees"] = in.Assignees
	}
	var gi githubIssue
	code, err := c.do(ctx, http.MethodPost, "/repos/"+repo+"/issues", body, &gi)
	if err != nil {
		return forge.IssueRef{}, err
	}
	if code/100 != 2 {
		return forge.IssueRef{}, statusErr("create issue", code)
	}
	return gi.toRef(), nil
}

// UpdateIssue applies a partial update; nil patch fields are left untouched.
func (c *AdminClient) UpdateIssue(ctx context.Context, repo string, number int, patch forge.IssuePatch) (forge.IssueRef, error) {
	body := map[string]any{}
	if patch.Title != nil {
		body["title"] = *patch.Title
	}
	if patch.Body != nil {
		body["body"] = *patch.Body
	}
	if patch.State != nil {
		body["state"] = *patch.State
	}
	if patch.Labels != nil {
		body["labels"] = *patch.Labels
	}
	if patch.Assignees != nil {
		body["assignees"] = *patch.Assignees
	}
	var gi githubIssue
	code, err := c.do(ctx, http.MethodPatch, "/repos/"+repo+"/issues/"+strconv.Itoa(number), body, &gi)
	if err != nil {
		return forge.IssueRef{}, err
	}
	if code/100 != 2 {
		return forge.IssueRef{}, statusErr("update issue", code)
	}
	return gi.toRef(), nil
}

// githubComment is the slice of GitHub's issue-comment object we normalize.
type githubComment struct {
	ID      int64  `json:"id"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
	User    struct {
		Login string `json:"login"`
	} `json:"user"`
	CreatedAt time.Time `json:"created_at"`
}

// CommentIssue posts a comment on an issue (or PR — GitHub shares the endpoint).
func (c *AdminClient) CommentIssue(ctx context.Context, repo string, number int, body string) (forge.CommentRef, error) {
	var gc githubComment
	code, err := c.do(ctx, http.MethodPost, "/repos/"+repo+"/issues/"+strconv.Itoa(number)+"/comments", map[string]any{"body": body}, &gc)
	if err != nil {
		return forge.CommentRef{}, err
	}
	if code/100 != 2 {
		return forge.CommentRef{}, statusErr("create comment", code)
	}
	return forge.CommentRef{
		ID:        strconv.FormatInt(gc.ID, 10),
		URL:       gc.HTMLURL,
		Body:      gc.Body,
		Author:    gc.User.Login,
		CreatedAt: gc.CreatedAt,
	}, nil
}

// AppClient's half of forge.IssueClient — the one an App connection resolves
// to, so a capability implemented only on *AdminClient is INVISIBLE to every
// `admin.(forge.IssueClient)` the server does. That is not a fringe shape: the
// studio's GitHub connect wizard creates App connections by default, and the
// forge→board sync, the board's push-to-forge routes and the autofix lane's
// hold-label veto all read the issue API through that assertion.
var _ forge.IssueClient = (*AppClient)(nil)

// issuesREST resolves the narrowest installation token that can serve an issue
// call: read for the two readers, write for the two writers. Every method
// below goes through it (or, for a comment, through its own profile), so no
// reader can drift onto a write token — and none of them rides the cached
// management token, which additionally carries contents/repository_hooks
// writes an issue call has no use for.
//
// A comment is deliberately NOT one of these two: it needs pull_requests as
// well, since the issues endpoint serves a PR's comments too — see
// IssueCommentInstallationPermissions.
func (a *AppClient) issuesREST(ctx context.Context, write bool) (*AdminClient, error) {
	if write {
		return a.scopedREST(ctx, IssuesWriteInstallationPermissions())
	}
	return a.scopedREST(ctx, IssuesReadInstallationPermissions())
}

func (a *AppClient) ListIssues(ctx context.Context, repo string, opts forge.IssueListOptions) ([]forge.IssueRef, error) {
	c, err := a.issuesREST(ctx, false)
	if err != nil {
		return nil, err
	}
	return c.ListIssues(ctx, repo, opts)
}

func (a *AppClient) GetIssue(ctx context.Context, repo string, number int) (forge.IssueRef, error) {
	c, err := a.issuesREST(ctx, false)
	if err != nil {
		return forge.IssueRef{}, err
	}
	return c.GetIssue(ctx, repo, number)
}

func (a *AppClient) CreateIssue(ctx context.Context, repo string, in forge.NewIssue) (forge.IssueRef, error) {
	c, err := a.issuesREST(ctx, true)
	if err != nil {
		return forge.IssueRef{}, err
	}
	return c.CreateIssue(ctx, repo, in)
}

func (a *AppClient) UpdateIssue(ctx context.Context, repo string, number int, patch forge.IssuePatch) (forge.IssueRef, error) {
	c, err := a.issuesREST(ctx, true)
	if err != nil {
		return forge.IssueRef{}, err
	}
	return c.UpdateIssue(ctx, repo, number, patch)
}

// CommentIssue rides its own scoped token — a third cached permission set,
// through the same chokepoint — because `number` may be a pull request, which
// GitHub gates on pull_requests even though the endpoint is the issues one.
func (a *AppClient) CommentIssue(ctx context.Context, repo string, number int, body string) (forge.CommentRef, error) {
	c, err := a.scopedREST(ctx, IssueCommentInstallationPermissions())
	if err != nil {
		return forge.CommentRef{}, err
	}
	return c.CommentIssue(ctx, repo, number, body)
}
