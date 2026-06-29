package forgejo

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// IssueClient: Forgejo/Gitea issue read/write powering forge→board sync
// (ListIssues) and board→forge push (Create/UpdateIssue).
//
// API base is "<BaseURL>/api/v1"; auth is the Gitea `token` scheme wired by
// AdminClient.http(). Like GitHub, the issues endpoint conflates issues and
// pull requests — an item with a non-null `pull_request` field is a PR, so
// IssueRef.IsPullRequest is set accordingly (callers filter on it).
var _ forge.IssueClient = (*AdminClient)(nil)

// forgejoUser is the shared {login} shape for poster/assignee.
type forgejoUser struct {
	Login string `json:"login"`
}

type forgejoLabel struct {
	Name string `json:"name"`
}

// forgejoPRMeta is the `pull_request` field on an issue (non-null ⇒ the item
// is a PR, not a plain issue).
type forgejoPRMeta struct {
	Merged  bool   `json:"merged"`
	HTMLURL string `json:"html_url"`
}

// forgejoIssue mirrors the Gitea API Issue shape (subset we normalize).
type forgejoIssue struct {
	Number      int64          `json:"number"`
	Title       string         `json:"title"`
	Body        string         `json:"body"`
	State       string         `json:"state"` // "open" | "closed"
	HTMLURL     string         `json:"html_url"`
	Poster      *forgejoUser   `json:"user"`
	Labels      []forgejoLabel `json:"labels"`
	Assignees   []forgejoUser  `json:"assignees"`
	PullRequest *forgejoPRMeta `json:"pull_request"`
	Created     time.Time      `json:"created_at"`
	Updated     time.Time      `json:"updated_at"`
}

func (i forgejoIssue) toRef() forge.IssueRef {
	ref := forge.IssueRef{
		Number:        int(i.Number),
		Title:         i.Title,
		Body:          i.Body,
		State:         i.State,
		URL:           i.HTMLURL,
		IsPullRequest: i.PullRequest != nil,
		CreatedAt:     i.Created,
		UpdatedAt:     i.Updated,
	}
	if i.Poster != nil {
		ref.Author = i.Poster.Login
	}
	for _, l := range i.Labels {
		ref.Labels = append(ref.Labels, l.Name)
	}
	for _, a := range i.Assignees {
		ref.Assignees = append(ref.Assignees, a.Login)
	}
	return ref
}

// ListIssues lists issues (and, conflated by Gitea, pull requests) for a repo.
// state defaults to "open"; Since enables incremental sync.
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
	page := opts.Page
	if page <= 0 {
		page = 1
	}
	vals.Set("page", strconv.Itoa(page))
	perPage := opts.PerPage
	if perPage <= 0 {
		perPage = 50
	}
	vals.Set("limit", strconv.Itoa(perPage))
	if !opts.Since.IsZero() {
		vals.Set("since", opts.Since.UTC().Format(time.RFC3339))
	}

	var issues []forgejoIssue
	code, err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/issues?"+vals.Encode(), nil, &issues)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, statusErr("GET issues", code)
	}
	out := make([]forge.IssueRef, 0, len(issues))
	for _, i := range issues {
		out = append(out, i.toRef())
	}
	return out, nil
}

// GetIssue fetches one issue (or PR) by its index.
func (c *AdminClient) GetIssue(ctx context.Context, repo string, number int) (forge.IssueRef, error) {
	var i forgejoIssue
	code, err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/issues/"+strconv.Itoa(number), nil, &i)
	if err != nil {
		return forge.IssueRef{}, err
	}
	if code != http.StatusOK {
		return forge.IssueRef{}, statusErr("GET issue", code)
	}
	return i.toRef(), nil
}

// CreateIssue opens a new issue (board→forge push).
//
// QUIRK: Gitea's CreateIssueOption takes Labels as []int64 (label IDs), NOT
// names — so we cannot attach labels by name here without an extra
// name→ID resolution round-trip. Labels-by-name are therefore intentionally
// skipped on create; set them with a follow-up label call if needed. Assignees
// ARE accepted by login. The created issue's full shape is returned.
func (c *AdminClient) CreateIssue(ctx context.Context, repo string, in forge.NewIssue) (forge.IssueRef, error) {
	body := map[string]any{
		"title": in.Title,
		"body":  in.Body,
	}
	if len(in.Assignees) > 0 {
		body["assignees"] = in.Assignees
	}
	var i forgejoIssue
	code, err := c.do(ctx, http.MethodPost, "/repos/"+repo+"/issues", body, &i)
	if err != nil {
		return forge.IssueRef{}, err
	}
	if code/100 != 2 {
		return forge.IssueRef{}, statusErr("create issue", code)
	}
	return i.toRef(), nil
}

// UpdateIssue applies a partial patch (title/body/state/assignees). Nil patch
// fields are left untouched.
//
// Labels are not patched here for the same []int64 label-ID quirk as
// CreateIssue (PATCH /issues/{index} takes label IDs, not names).
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
	if patch.Assignees != nil {
		body["assignees"] = *patch.Assignees
	}
	var i forgejoIssue
	code, err := c.do(ctx, http.MethodPatch, "/repos/"+repo+"/issues/"+strconv.Itoa(number), body, &i)
	if err != nil {
		return forge.IssueRef{}, err
	}
	if code/100 != 2 {
		return forge.IssueRef{}, statusErr("update issue", code)
	}
	return i.toRef(), nil
}

// forgejoComment is the slice of Gitea's issue-comment object we normalize.
type forgejoComment struct {
	ID        int64        `json:"id"`
	HTMLURL   string       `json:"html_url"`
	Body      string       `json:"body"`
	Poster    *forgejoUser `json:"user"`
	CreatedAt time.Time    `json:"created_at"`
}

// CommentIssue posts a comment on an issue (or PR — Gitea shares the endpoint).
func (c *AdminClient) CommentIssue(ctx context.Context, repo string, number int, body string) (forge.CommentRef, error) {
	var fc forgejoComment
	code, err := c.do(ctx, http.MethodPost, "/repos/"+repo+"/issues/"+strconv.Itoa(number)+"/comments", map[string]any{"body": body}, &fc)
	if err != nil {
		return forge.CommentRef{}, err
	}
	if code/100 != 2 {
		return forge.CommentRef{}, statusErr("create comment", code)
	}
	ref := forge.CommentRef{
		ID:        strconv.FormatInt(fc.ID, 10),
		URL:       fc.HTMLURL,
		Body:      fc.Body,
		CreatedAt: fc.CreatedAt,
	}
	if fc.Poster != nil {
		ref.Author = fc.Poster.Login
	}
	return ref, nil
}
