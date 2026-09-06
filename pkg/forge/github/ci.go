package github

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// PullClient capability — linked PRs + CI status (current + history) for board
// cards. PRs come from /pulls; CI is the union of check-runs (GitHub Actions /
// Apps) and the legacy combined commit-status, normalized onto forge.CI*.
//
// Every refusal below names the grant GitHub gates the endpoint on (its
// published per-endpoint permission data — the same rule the App client's
// profiles mint), so a 403 "Resource not accessible by integration" reaches
// the operator as the permission to approve, not as a bare status.
var _ forge.PullClient = (*AdminClient)(nil)

// githubPull is the slice of the GitHub pull-request object we map to PullRef.
type githubPull struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	State   string `json:"state"` // "open" | "closed"
	HTMLURL string `json:"html_url"`
	Draft   bool   `json:"draft"`
	Head    struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	MergedAt  *time.Time `json:"merged_at"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

func (gp githubPull) toRef() forge.PullRef {
	state := gp.State
	if gp.MergedAt != nil {
		state = "merged"
	}
	return forge.PullRef{
		Number:           gp.Number,
		Title:            gp.Title,
		State:            state,
		URL:              gp.HTMLURL,
		SourceBranch:     gp.Head.Ref,
		TargetBranch:     gp.Base.Ref,
		HeadSHA:          gp.Head.SHA,
		HeadRepoFullName: gp.Head.Repo.FullName,
		Author:           gp.User.Login,
		Draft:            gp.Draft,
		CreatedAt:        gp.CreatedAt,
		UpdatedAt:        gp.UpdatedAt,
		LinkedIssues:     forge.ParseIssueRefs(false, gp.Title, gp.Body),
	}
}

// ListPullRequests lists PRs for repo ("owner/repo"). GitHub's /pulls endpoint
// has no `since` filter, so opts.Since is ignored here.
func (c *AdminClient) ListPullRequests(ctx context.Context, repo string, opts forge.PullListOptions) ([]forge.PullRef, error) {
	vals := url.Values{}
	state := opts.State
	if state == "" {
		state = "open"
	}
	vals.Set("state", state)
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

	var raw []githubPull
	code, errBody, err := c.doErr(ctx, http.MethodGet, "/repos/"+repo+"/pulls?"+vals.Encode(), nil, &raw)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, refusal("GET pulls", code, errBody, "pull_requests:read")
	}
	out := make([]forge.PullRef, 0, len(raw))
	for _, gp := range raw {
		out = append(out, gp.toRef())
	}
	return out, nil
}

// GetPullRequest fetches one PR by number. GitHub gates the single-PR read on
// contents read as well as pull_requests read (the object carries
// content-derived fields), unlike the collection read.
func (c *AdminClient) GetPullRequest(ctx context.Context, repo string, number int) (forge.PullRef, error) {
	var gp githubPull
	code, errBody, err := c.doErr(ctx, http.MethodGet, "/repos/"+repo+"/pulls/"+strconv.Itoa(number), nil, &gp)
	if err != nil {
		return forge.PullRef{}, err
	}
	if code != http.StatusOK {
		return forge.PullRef{}, refusal("GET pull", code, errBody, "pull_requests:read", "contents:read")
	}
	return gp.toRef(), nil
}

// githubCheckRuns is the check-runs list response (GitHub Actions / Apps).
type githubCheckRuns struct {
	CheckRuns []struct {
		Name        string     `json:"name"`
		Status      string     `json:"status"`     // queued | in_progress | completed
		Conclusion  string     `json:"conclusion"` // success | failure | cancelled | skipped | timed_out | neutral | action_required
		HTMLURL     string     `json:"html_url"`
		StartedAt   *time.Time `json:"started_at"`
		CompletedAt *time.Time `json:"completed_at"`
	} `json:"check_runs"`
}

// githubCombinedStatus is the legacy combined commit-status response.
type githubCombinedStatus struct {
	State    string `json:"state"` // success | failure | pending
	SHA      string `json:"sha"`
	Statuses []struct {
		Context   string    `json:"context"`
		State     string    `json:"state"` // success | failure | pending | error
		TargetURL string    `json:"target_url"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
	} `json:"statuses"`
}

// checkRunStatus normalizes a check-run (status, conclusion) onto a forge.CI*
// value. A completed run reports its conclusion; an in-flight one is running.
func checkRunStatus(status, conclusion string) string {
	switch status {
	case "queued", "in_progress":
		return forge.CIRunning
	case "completed":
		switch conclusion {
		case "success":
			return forge.CISuccess
		case "failure", "timed_out", "action_required":
			return forge.CIFailed
		case "cancelled":
			return forge.CICancelled
		case "skipped", "neutral":
			return forge.CISkipped
		default:
			return forge.CIUnknown
		}
	default:
		return forge.CIUnknown
	}
}

// commitStatusState normalizes a combined/individual commit-status state onto
// a forge.CI* value.
func commitStatusState(state string) string {
	switch state {
	case "success":
		return forge.CISuccess
	case "failure", "error":
		return forge.CIFailed
	case "pending":
		return forge.CIRunning
	default:
		return forge.CIUnknown
	}
}

func tm(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return *p
}

// fetchCheckRuns returns the normalized check-runs for a ref (empty on 404,
// which a repo without GitHub Actions returns).
func (c *AdminClient) fetchCheckRuns(ctx context.Context, repo, ref string) ([]forge.CIRun, error) {
	var cr githubCheckRuns
	code, errBody, err := c.doErr(ctx, http.MethodGet, "/repos/"+repo+"/commits/"+url.PathEscape(ref)+"/check-runs", nil, &cr)
	if err != nil {
		return nil, err
	}
	if code == http.StatusNotFound {
		return nil, nil
	}
	if code != http.StatusOK {
		return nil, refusal("GET check-runs", code, errBody, "checks:read")
	}
	runs := make([]forge.CIRun, 0, len(cr.CheckRuns))
	for _, r := range cr.CheckRuns {
		runs = append(runs, forge.CIRun{
			Name:       r.Name,
			Status:     checkRunStatus(r.Status, r.Conclusion),
			Conclusion: r.Conclusion,
			URL:        r.HTMLURL,
			SHA:        ref,
			StartedAt:  tm(r.StartedAt),
			FinishedAt: tm(r.CompletedAt),
		})
	}
	return runs, nil
}

// fetchCommitStatuses returns the normalized legacy commit-statuses for a ref.
func (c *AdminClient) fetchCommitStatuses(ctx context.Context, repo, ref string) (sha string, _ []forge.CIRun, _ error) {
	var cs githubCombinedStatus
	code, errBody, err := c.doErr(ctx, http.MethodGet, "/repos/"+repo+"/commits/"+url.PathEscape(ref)+"/status", nil, &cs)
	if err != nil {
		return "", nil, err
	}
	if code == http.StatusNotFound {
		return "", nil, nil
	}
	if code != http.StatusOK {
		return "", nil, refusal("GET commit status", code, errBody, "statuses:read")
	}
	runs := make([]forge.CIRun, 0, len(cs.Statuses))
	for _, s := range cs.Statuses {
		runs = append(runs, forge.CIRun{
			Name:       s.Context,
			Status:     commitStatusState(s.State),
			URL:        s.TargetURL,
			SHA:        cs.SHA,
			StartedAt:  s.CreatedAt,
			FinishedAt: s.UpdatedAt,
		})
	}
	return cs.SHA, runs, nil
}

// aggregateState rolls a set of normalized runs into a single CIStatus.State:
// any failure → failed; any running/queued → running; all success → success;
// none → unknown; otherwise pending.
func aggregateState(runs []forge.CIRun) string {
	if len(runs) == 0 {
		return forge.CIUnknown
	}
	anyRunning, anySuccess, anyUnknown := false, false, false
	for _, r := range runs {
		switch r.Status {
		case forge.CIFailed:
			// Any hard failure dominates the aggregate.
			return forge.CIFailed
		case forge.CIRunning, forge.CIPending:
			anyRunning = true
		case forge.CISuccess:
			anySuccess = true
		case forge.CISkipped, forge.CICancelled:
			// Non-blocking neutral states (GitHub treats skipped/cancelled
			// as not failing the ref); they neither succeed nor block.
		default:
			anyUnknown = true
		}
	}
	switch {
	case anyRunning:
		return forge.CIRunning
	case anySuccess && !anyUnknown:
		// At least one success and no unresolved state → green.
		return forge.CISuccess
	case anyUnknown:
		return forge.CIUnknown
	default:
		// All runs were neutral (skipped/cancelled) — nothing to report green.
		return forge.CIPending
	}
}

// GetCIStatus returns the CURRENT aggregate CI state + runs for a ref,
// combining GitHub Actions/App check-runs with the legacy commit-status API.
func (c *AdminClient) GetCIStatus(ctx context.Context, repo, ref string) (forge.CIStatus, error) {
	checkRuns, err := c.fetchCheckRuns(ctx, repo, ref)
	if err != nil {
		return forge.CIStatus{}, err
	}
	sha, statusRuns, err := c.fetchCommitStatuses(ctx, repo, ref)
	if err != nil {
		return forge.CIStatus{}, err
	}
	runs := append(checkRuns, statusRuns...)
	if sha == "" {
		sha = ref
	}
	return forge.CIStatus{
		SHA:   sha,
		State: aggregateState(runs),
		Runs:  runs,
	}, nil
}

// ListCIHistory lists recent check-runs for a ref/branch, newest first (by
// start time), capped at limit (0 → 30).
func (c *AdminClient) ListCIHistory(ctx context.Context, repo, ref string, limit int) ([]forge.CIRun, error) {
	if limit <= 0 {
		limit = 30
	}
	runs, err := c.fetchCheckRuns(ctx, repo, ref)
	if err != nil {
		return nil, err
	}
	// Newest first by start time (check-runs come latest-filtered but unsorted).
	sortRunsNewestFirst(runs)
	if len(runs) > limit {
		runs = runs[:limit]
	}
	return runs, nil
}

// sortRunsNewestFirst orders runs by StartedAt descending (zero times last).
func sortRunsNewestFirst(runs []forge.CIRun) {
	for i := 1; i < len(runs); i++ {
		for j := i; j > 0 && runs[j].StartedAt.After(runs[j-1].StartedAt); j-- {
			runs[j], runs[j-1] = runs[j-1], runs[j]
		}
	}
}

// CreatePull opens a pull request. head/base are branch names; Draft maps to
// GitHub's `draft` create flag.
func (c *AdminClient) CreatePull(ctx context.Context, repo string, in forge.NewPull) (forge.PullRef, error) {
	body := map[string]any{
		"title": in.Title,
		"head":  in.SourceBranch,
		"base":  in.TargetBranch,
	}
	if in.Body != "" {
		body["body"] = in.Body
	}
	if in.Draft {
		body["draft"] = true
	}
	var gp githubPull
	code, errBody, err := c.doErr(ctx, http.MethodPost, "/repos/"+repo+"/pulls", body, &gp)
	if err != nil {
		return forge.PullRef{}, err
	}
	if code/100 != 2 {
		return forge.PullRef{}, refusal("create pull", code, errBody, "pull_requests:write")
	}
	return gp.toRef(), nil
}

// UpdatePull applies a partial update. GitHub's REST PATCH covers title/body/
// base/state; converting draft↔ready is GraphQL-only, so PullPatch carries no
// draft toggle.
func (c *AdminClient) UpdatePull(ctx context.Context, repo string, number int, patch forge.PullPatch) (forge.PullRef, error) {
	body := map[string]any{}
	if patch.Title != nil {
		body["title"] = *patch.Title
	}
	if patch.Body != nil {
		body["body"] = *patch.Body
	}
	if patch.TargetBranch != nil {
		body["base"] = *patch.TargetBranch
	}
	if patch.State != nil {
		body["state"] = *patch.State
	}
	var gp githubPull
	code, errBody, err := c.doErr(ctx, http.MethodPatch, "/repos/"+repo+"/pulls/"+strconv.Itoa(number), body, &gp)
	if err != nil {
		return forge.PullRef{}, err
	}
	if code/100 != 2 {
		return forge.PullRef{}, refusal("update pull", code, errBody, "pull_requests:write")
	}
	return gp.toRef(), nil
}

// MergePull merges a PR via PUT /pulls/{n}/merge, then re-fetches it once so the
// returned ref reflects the merged state. When opts.DeleteBranch is set, the
// source branch (read off the re-fetched ref) is best-effort deleted afterwards
// — a failure there does not fail the merge. GitHub gates the merge on
// contents write (it writes the base branch), not on pull_requests write: the
// row under Pull requests for this path is the GET ("check if a pull request
// has been merged"), a different endpoint — see
// PullMergeInstallationPermissions for the two rows side by side.
func (c *AdminClient) MergePull(ctx context.Context, repo string, number int, opts forge.MergeOptions) (forge.PullRef, error) {
	body := map[string]any{"merge_method": forge.MergeMethodWire(opts.Method)}
	if opts.CommitTitle != "" {
		body["commit_title"] = opts.CommitTitle
	}
	if opts.CommitMessage != "" {
		body["commit_message"] = opts.CommitMessage
	}
	if opts.SHA != "" {
		body["sha"] = opts.SHA
	}
	code, errBody, err := c.doErr(ctx, http.MethodPut, "/repos/"+repo+"/pulls/"+strconv.Itoa(number)+"/merge", body, nil)
	if err != nil {
		return forge.PullRef{}, err
	}
	if code/100 != 2 {
		return forge.PullRef{}, refusal("merge pull", code, errBody, "contents:write")
	}
	merged, err := c.GetPullRequest(ctx, repo, number)
	if err != nil {
		// Merge succeeded; synthesize a minimal merged ref rather than fail.
		return forge.PullRef{State: "merged", Number: number}, nil
	}
	if opts.DeleteBranch && merged.SourceBranch != "" {
		// Best-effort: ignore errors (branch may be auto-deleted, protected, or
		// already gone). The merge already succeeded.
		_, _ = c.do(ctx, http.MethodDelete, "/repos/"+repo+"/git/refs/heads/"+url.PathEscape(merged.SourceBranch), nil, nil)
	}
	return merged, nil
}
