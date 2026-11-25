package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Run is one workflow run as GET /actions/runs/{id} reports it: the fields
// describe its LATEST attempt (run_attempt, status, conclusion).
type Run struct {
	ID             int64     `json:"id"`
	RunNumber      int       `json:"run_number"`
	Attempt        int       `json:"run_attempt"`
	Status         string    `json:"status"`
	Conclusion     string    `json:"conclusion"`
	Event          string    `json:"event"`
	HeadBranch     string    `json:"head_branch"`
	HeadSHA        string    `json:"head_sha"`
	Path           string    `json:"path"`
	HTMLURL        string    `json:"html_url"`
	CreatedAt      time.Time `json:"created_at"`
	HeadRepository struct {
		FullName string `json:"full_name"`
	} `json:"head_repository"`
}

// Job is one job of one run attempt.
type Job struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
}

// PullRequest carries what the revert decision reads.
type PullRequest struct {
	Number         int        `json:"number"`
	NodeID         string     `json:"node_id"`
	Title          string     `json:"title"`
	State          string     `json:"state"`
	HTMLURL        string     `json:"html_url"`
	MergedAt       *time.Time `json:"merged_at"`
	MergeCommitSHA string     `json:"merge_commit_sha"`
	Head           struct {
		Ref string `json:"ref"`
	} `json:"head"`
	// AutoMerge is nil while auto-merge is not armed.
	AutoMerge *struct{} `json:"auto_merge"`
}

// User is the author of an issue or a comment.
type User struct {
	Login string `json:"login"`
}

// Issue is an issue (never a pull request: the listing drops those).
type Issue struct {
	ID          int64     `json:"id"`
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	CreatedAt   time.Time `json:"created_at"`
	PullRequest *struct{} `json:"pull_request,omitempty"`
	User        User      `json:"user"`
	// AuthorAssociation is the author's relation to the repository: OWNER,
	// MEMBER, COLLABORATOR, CONTRIBUTOR, NONE…
	AuthorAssociation string `json:"author_association"`
}

// Comment is one issue or pull request comment.
type Comment struct {
	Body string `json:"body"`
	User User   `json:"user"`
}

// Commit is one commit of a comparison.
type Commit struct {
	SHA     string `json:"sha"`
	Message string `json:"message"`
}

// GitHub is every call main-watch makes. The reads come first; every method
// after CreateIssue writes, and dryRun replaces exactly those.
type GitHub interface {
	Run(ctx context.Context, id int64) (Run, error)
	Attempt(ctx context.Context, id int64, attempt int) (Run, error)
	Jobs(ctx context.Context, id int64, attempt int) ([]Job, error)
	JobLog(ctx context.Context, jobID int64) (string, error)
	// MainRuns lists the newest push and schedule runs of the watched
	// workflow on main, whatever their status or head repository: callers
	// filter through eligible.
	MainRuns(ctx context.Context) ([]Run, error)
	Compare(ctx context.Context, base, head string) ([]Commit, error)
	PullRequest(ctx context.Context, number int) (PullRequest, error)
	OpenIssues(ctx context.Context) ([]Issue, error)
	Comments(ctx context.Context, number int) ([]Comment, error)
	MainSHA(ctx context.Context) (string, error)
	// File returns a file's content and blob sha at a commit.
	File(ctx context.Context, path, ref string) ([]byte, string, error)
	BranchExists(ctx context.Context, branch string) (bool, error)
	// PullRequestFrom returns the newest pull request, in any state, whose
	// head is branch of this repository, or ok=false when there is none.
	PullRequestFrom(ctx context.Context, branch string) (pr PullRequest, ok bool, err error)

	RerunFailedJobs(ctx context.Context, id int64) error
	EnsureLabel(ctx context.Context, name, color, description string) error
	CreateIssue(ctx context.Context, title, body string, labels []string) (Issue, error)
	AddSubIssue(ctx context.Context, epic int, issueID int64) error
	Comment(ctx context.Context, number int, body string) error
	CloseIssue(ctx context.Context, number int) error
	// The writes below run under the bot token: a branch, a commit or a pull
	// request made with GITHUB_TOKEN starts no workflow, so the pull request
	// would never get the checks its merge needs.
	CreateBranch(ctx context.Context, branch, sha string) error
	PutFile(ctx context.Context, branch, path, message string, content []byte, blobSHA string) error
	CreatePullRequest(ctx context.Context, head, title, body string) (PullRequest, error)
	EnableAutoMerge(ctx context.Context, pr PullRequest) error
	ClosePullRequest(ctx context.Context, number int) error
}

// restClient talks to the GitHub REST and GraphQL APIs of one repository.
type restClient struct {
	api, graphql, repo, workflow string
	token, botToken              string
	http                         *http.Client
}

func (c *restClient) do(ctx context.Context, token, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("%s %s: encode request: %w", method, path, err)
		}
		body = bytes.NewReader(buf)
	}
	target := path
	if !strings.Contains(path, "://") {
		target = c.api + path
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s %s: read response: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &httpError{Method: method, Path: path, Status: resp.StatusCode, Body: excerpt(string(raw))}
	}
	if out == nil {
		return nil
	}
	if s, ok := out.(*string); ok {
		*s = string(raw)
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return nil
}

// httpError is a non-2xx answer; callers that expect one status (404 on a
// probe) read it with errors.As.
type httpError struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Body)
}

func excerpt(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 500 {
		return s[:500] + "…"
	}
	return s
}

func (c *restClient) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, c.token, http.MethodGet, path, nil, out)
}

func (c *restClient) repoPath(format string, a ...any) string {
	return "/repos/" + c.repo + fmt.Sprintf(format, a...)
}

func (c *restClient) Run(ctx context.Context, id int64) (Run, error) {
	var r Run
	return r, c.get(ctx, c.repoPath("/actions/runs/%d", id), &r)
}

func (c *restClient) Attempt(ctx context.Context, id int64, attempt int) (Run, error) {
	var r Run
	return r, c.get(ctx, c.repoPath("/actions/runs/%d/attempts/%d", id, attempt), &r)
}

// pages fetches a paginated listing until a short page, at most max pages.
func pages[T any](ctx context.Context, c *restClient, path string, max int, field func(raw json.RawMessage) ([]T, error)) ([]T, error) {
	var all []T
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	for page := 1; page <= max; page++ {
		var raw json.RawMessage
		if err := c.get(ctx, fmt.Sprintf("%s%sper_page=100&page=%d", path, sep, page), &raw); err != nil {
			return nil, err
		}
		items, err := field(raw)
		if err != nil {
			return nil, fmt.Errorf("GET %s: decode page %d: %w", path, page, err)
		}
		all = append(all, items...)
		if len(items) < 100 {
			return all, nil
		}
	}
	return nil, fmt.Errorf("GET %s: more than %d pages: refusing a partial listing", path, max)
}

func (c *restClient) Jobs(ctx context.Context, id int64, attempt int) ([]Job, error) {
	return pages(ctx, c, c.repoPath("/actions/runs/%d/attempts/%d/jobs", id, attempt), 5, func(raw json.RawMessage) ([]Job, error) {
		var v struct {
			Jobs []Job `json:"jobs"`
		}
		err := json.Unmarshal(raw, &v)
		return v.Jobs, err
	})
}

func (c *restClient) JobLog(ctx context.Context, jobID int64) (string, error) {
	var s string
	return s, c.get(ctx, c.repoPath("/actions/jobs/%d/logs", jobID), &s)
}

// MainRuns reads the first page of push runs and of schedule runs (the
// nightly), the newest 100 of each: a red episode older than that finds no
// last green run, and its ticket says the suspects are unknown rather than
// guessing them. The `branch` filter matches a run's head branch, which a
// pull request from any fork's `main` also has, so the server is asked for
// push and schedule runs by event: such pull request runs cannot fill a page.
// A workflow_dispatch run is left out: main-watch.yml never receives one, so
// the listing judges the same runs the event path does. A full page of push
// runs is the window: nightlies older than its oldest run are dropped, so the
// last-green lookup never reaches past push runs the page cut off.
func (c *restClient) MainRuns(ctx context.Context) ([]Run, error) {
	list := func(event string) ([]Run, error) {
		var v struct {
			Runs []Run `json:"workflow_runs"`
		}
		err := c.get(ctx, c.repoPath("/actions/workflows/%s/runs?branch=main&event=%s&per_page=100", url.PathEscape(c.workflow), event), &v)
		return v.Runs, err
	}
	push, err := list("push")
	if err != nil {
		return nil, err
	}
	nightly, err := list("schedule")
	if err != nil {
		return nil, err
	}
	oldest := 0
	if len(push) == 100 {
		oldest = slices.MinFunc(push, func(a, b Run) int { return a.RunNumber - b.RunNumber }).RunNumber
	}
	for _, r := range nightly {
		if r.RunNumber >= oldest {
			push = append(push, r)
		}
	}
	return push, nil
}

func (c *restClient) Compare(ctx context.Context, base, head string) ([]Commit, error) {
	var v struct {
		TotalCommits int `json:"total_commits"`
		Commits      []struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message string `json:"message"`
			} `json:"commit"`
		} `json:"commits"`
	}
	if err := c.get(ctx, c.repoPath("/compare/%s...%s?per_page=250", base, head), &v); err != nil {
		return nil, err
	}
	if v.TotalCommits != len(v.Commits) {
		return nil, fmt.Errorf("compare %s...%s: %d commits listed of %d: refusing a partial suspect list", base, head, len(v.Commits), v.TotalCommits)
	}
	out := make([]Commit, len(v.Commits))
	for i, cm := range v.Commits {
		out[i] = Commit{SHA: cm.SHA, Message: cm.Commit.Message}
	}
	return out, nil
}

func (c *restClient) PullRequest(ctx context.Context, number int) (PullRequest, error) {
	var pr PullRequest
	return pr, c.get(ctx, c.repoPath("/pulls/%d", number), &pr)
}

func (c *restClient) OpenIssues(ctx context.Context) ([]Issue, error) {
	all, err := pages(ctx, c, c.repoPath("/issues?state=open"), 20, func(raw json.RawMessage) ([]Issue, error) {
		var v []Issue
		err := json.Unmarshal(raw, &v)
		return v, err
	})
	if err != nil {
		return nil, err
	}
	var issues []Issue
	for _, is := range all {
		if is.PullRequest == nil {
			issues = append(issues, is)
		}
	}
	return issues, nil
}

func (c *restClient) Comments(ctx context.Context, number int) ([]Comment, error) {
	return pages(ctx, c, c.repoPath("/issues/%d/comments", number), 20, func(raw json.RawMessage) ([]Comment, error) {
		var v []Comment
		err := json.Unmarshal(raw, &v)
		return v, err
	})
}

func (c *restClient) MainSHA(ctx context.Context) (string, error) {
	var v struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := c.get(ctx, c.repoPath("/git/ref/heads/main"), &v); err != nil {
		return "", err
	}
	if v.Object.SHA == "" {
		return "", fmt.Errorf("GET refs/heads/main: no sha in the answer")
	}
	return v.Object.SHA, nil
}

func (c *restClient) File(ctx context.Context, path, ref string) ([]byte, string, error) {
	var v struct {
		SHA      string `json:"sha"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if err := c.get(ctx, c.repoPath("/contents/%s?ref=%s", path, url.QueryEscape(ref)), &v); err != nil {
		return nil, "", err
	}
	if v.Encoding != "base64" {
		return nil, "", fmt.Errorf("contents %s@%s: encoding %q, want base64", path, ref, v.Encoding)
	}
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(v.Content, "\n", ""))
	if err != nil {
		return nil, "", fmt.Errorf("contents %s@%s: %w", path, ref, err)
	}
	return data, v.SHA, nil
}

func (c *restClient) BranchExists(ctx context.Context, branch string) (bool, error) {
	err := c.get(ctx, c.repoPath("/git/ref/heads/%s", branch), nil)
	if err == nil {
		return true, nil
	}
	if he := (*httpError)(nil); errors.As(err, &he) && he.Status == http.StatusNotFound {
		return false, nil
	}
	return false, err
}

func (c *restClient) PullRequestFrom(ctx context.Context, branch string) (PullRequest, bool, error) {
	owner, _, _ := strings.Cut(c.repo, "/")
	var prs []PullRequest
	if err := c.get(ctx, c.repoPath("/pulls?state=all&head=%s&sort=created&direction=desc&per_page=1", url.QueryEscape(owner+":"+branch)), &prs); err != nil {
		return PullRequest{}, false, err
	}
	if len(prs) == 0 {
		return PullRequest{}, false, nil
	}
	return prs[0], true, nil
}

func (c *restClient) RerunFailedJobs(ctx context.Context, id int64) error {
	return c.do(ctx, c.token, http.MethodPost, c.repoPath("/actions/runs/%d/rerun-failed-jobs", id), struct{}{}, nil)
}

func (c *restClient) EnsureLabel(ctx context.Context, name, color, description string) error {
	err := c.get(ctx, c.repoPath("/labels/%s", url.PathEscape(name)), nil)
	if err == nil {
		return nil
	}
	if he := (*httpError)(nil); !errors.As(err, &he) || he.Status != http.StatusNotFound {
		return err
	}
	return c.do(ctx, c.token, http.MethodPost, c.repoPath("/labels"), map[string]string{"name": name, "color": color, "description": description}, nil)
}

func (c *restClient) CreateIssue(ctx context.Context, title, body string, labels []string) (Issue, error) {
	var is Issue
	return is, c.do(ctx, c.token, http.MethodPost, c.repoPath("/issues"), map[string]any{"title": title, "body": body, "labels": labels}, &is)
}

func (c *restClient) AddSubIssue(ctx context.Context, epic int, issueID int64) error {
	return c.do(ctx, c.token, http.MethodPost, c.repoPath("/issues/%d/sub_issues", epic), map[string]int64{"sub_issue_id": issueID}, nil)
}

// Comment runs under the job's GITHUB_TOKEN, on an issue or a pull request:
// GitHub gates the second on the token's pull-requests permission, which the
// workflow grants.
func (c *restClient) Comment(ctx context.Context, number int, body string) error {
	return c.do(ctx, c.token, http.MethodPost, c.repoPath("/issues/%d/comments", number), map[string]string{"body": body}, nil)
}

func (c *restClient) CloseIssue(ctx context.Context, number int) error {
	return c.do(ctx, c.token, http.MethodPatch, c.repoPath("/issues/%d", number), map[string]string{"state": "closed", "state_reason": "completed"}, nil)
}

func (c *restClient) CreateBranch(ctx context.Context, branch, sha string) error {
	return c.do(ctx, c.botToken, http.MethodPost, c.repoPath("/git/refs"), map[string]string{"ref": "refs/heads/" + branch, "sha": sha}, nil)
}

func (c *restClient) PutFile(ctx context.Context, branch, path, message string, content []byte, blobSHA string) error {
	return c.do(ctx, c.botToken, http.MethodPut, c.repoPath("/contents/%s", path), map[string]string{
		"message": message,
		"content": base64.StdEncoding.EncodeToString(content),
		"sha":     blobSHA,
		"branch":  branch,
	}, nil)
}

func (c *restClient) CreatePullRequest(ctx context.Context, head, title, body string) (PullRequest, error) {
	var pr PullRequest
	return pr, c.do(ctx, c.botToken, http.MethodPost, c.repoPath("/pulls"), map[string]string{"head": head, "base": "main", "title": title, "body": body}, &pr)
}

// EnableAutoMerge arms auto-merge with no merge method: on a branch behind a
// merge queue the queue owns the method, and GitHub enqueues the pull request
// once its required checks pass.
func (c *restClient) EnableAutoMerge(ctx context.Context, pr PullRequest) error {
	var v struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	q := map[string]any{
		"query":     `mutation($id: ID!) { enablePullRequestAutoMerge(input: {pullRequestId: $id}) { clientMutationId } }`,
		"variables": map[string]string{"id": pr.NodeID},
	}
	if err := c.do(ctx, c.botToken, http.MethodPost, c.graphql, q, &v); err != nil {
		return err
	}
	if len(v.Errors) > 0 {
		msgs := make([]string, len(v.Errors))
		for i, e := range v.Errors {
			msgs[i] = e.Message
		}
		return fmt.Errorf("enable auto-merge on #%d: %s", pr.Number, strings.Join(msgs, "; "))
	}
	return nil
}

// ClosePullRequest runs under the bot token, the App that opened the pull
// request.
func (c *restClient) ClosePullRequest(ctx context.Context, number int) error {
	return c.do(ctx, c.botToken, http.MethodPatch, c.repoPath("/pulls/%d", number), map[string]string{"state": "closed"}, nil)
}
