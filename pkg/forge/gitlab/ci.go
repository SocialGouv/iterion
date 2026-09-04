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

// gitlabMR is the GitLab merge-request shape (read). Like issues, an MR is
// addressed by its per-project `iid`; `state` is "opened"/"merged"/"closed",
// `sha` is the head commit, `work_in_progress`/`draft` flag a draft.
type gitlabMR struct {
	IID            int        `json:"iid"`
	Title          string     `json:"title"`
	Description    string     `json:"description"`
	State          string     `json:"state"`
	WebURL         string     `json:"web_url"`
	SourceBranch   string     `json:"source_branch"`
	TargetBranch   string     `json:"target_branch"`
	SHA            string     `json:"sha"`
	Draft          bool       `json:"draft"`
	WorkInProgress bool       `json:"work_in_progress"`
	Author         gitlabUser `json:"author"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	// GitLab names the two sides of an MR by numeric project id only — there
	// is no `head.repo.full_name` twin of the GitHub/Forgejo payload. Equal
	// ids mean the source branch lives in the very project the MR was queried
	// under, which is what proves "same repo" for toRef.
	SourceProjectID int `json:"source_project_id"`
	TargetProjectID int `json:"target_project_id"`
}

// toRef normalizes a GitLab MR onto forge.PullRef. state "opened"→"open",
// "merged"/"closed" pass through; draft is the OR of the two GitLab flags.
//
// repo is the project path the MR was addressed under (GitLab scopes an MR
// iid to its TARGET project, so repo is that project). HeadRepoFullName is
// filled with it only when source_project_id equals target_project_id: GitLab
// gives no path for the source project, so equality with the project we just
// queried is the only head-repo identity this payload proves. A fork (ids
// differ) or an older/partial payload (either id zero) leaves it EMPTY, which
// every fail-closed caller — PullRef.SameRepoAs, the command / gate-autofix /
// gate-relaunch guards — reads as "not proven same-repo" and refuses.
func (mr gitlabMR) toRef(repo string) forge.PullRef {
	ref := forge.PullRef{
		Number:       mr.IID,
		Title:        mr.Title,
		State:        normMRState(mr.State),
		URL:          mr.WebURL,
		SourceBranch: mr.SourceBranch,
		TargetBranch: mr.TargetBranch,
		HeadSHA:      mr.SHA,
		Author:       mr.Author.Username,
		Draft:        mr.Draft || mr.WorkInProgress,
		CreatedAt:    mr.CreatedAt,
		UpdatedAt:    mr.UpdatedAt,
		// GitLab has no first-class field for arbitrary issue linkage in the
		// MR payload, so LinkedIssues is parsed best-effort from title+body.
		LinkedIssues: forge.ParseIssueRefs(false, mr.Title, mr.Description),
	}
	if mr.SourceProjectID != 0 && mr.SourceProjectID == mr.TargetProjectID {
		ref.HeadRepoFullName = strings.TrimSpace(repo)
	}
	return ref
}

// normMRState maps GitLab's MR state onto the forge vocabulary:
// "opened"→"open"; "merged"/"closed" are already canonical.
func normMRState(s string) string {
	if s == "opened" {
		return "open"
	}
	return s
}

// mrStateQuery maps a forge PullListOptions.State onto GitLab's MR `state`
// query value; "all"/empty → no filter.
func mrStateQuery(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "open", "opened":
		return "opened"
	case "merged":
		return "merged"
	case "closed":
		return "closed"
	default:
		return ""
	}
}

// ListPullRequests lists a project's merge requests, normalized to forge.PullRef.
func (c *AdminClient) ListPullRequests(ctx context.Context, repo string, opts forge.PullListOptions) ([]forge.PullRef, error) {
	vals := url.Values{}
	if st := mrStateQuery(opts.State); st != "" {
		vals.Set("state", st)
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

	var mrs []gitlabMR
	code, err := c.do(ctx, http.MethodGet, "/projects/"+projectID(repo)+"/merge_requests?"+vals.Encode(), nil, &mrs)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, statusErr("list merge requests", code)
	}
	out := make([]forge.PullRef, 0, len(mrs))
	for _, mr := range mrs {
		out = append(out, mr.toRef(repo))
	}
	return out, nil
}

// GetPullRequest fetches one merge request by its per-project iid.
func (c *AdminClient) GetPullRequest(ctx context.Context, repo string, number int) (forge.PullRef, error) {
	var mr gitlabMR
	code, err := c.do(ctx, http.MethodGet, "/projects/"+projectID(repo)+"/merge_requests/"+strconv.Itoa(number), nil, &mr)
	if err != nil {
		return forge.PullRef{}, err
	}
	if code != http.StatusOK {
		return forge.PullRef{}, statusErr("get merge request", code)
	}
	return mr.toRef(repo), nil
}

// gitlabCommitStatus is one per-job commit status (the GitLab
// /repository/commits/:sha/statuses entry).
type gitlabCommitStatus struct {
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	SHA        string    `json:"sha"`
	TargetURL  string    `json:"target_url"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// normCIStatus maps a GitLab job/pipeline status onto the forge CI* vocabulary.
// GitLab's "canceled" (one L) and the queue states (created/preparing/…) are
// folded onto the canonical set; unknown values → CIUnknown.
func normCIStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "success":
		return forge.CISuccess
	case "failed":
		return forge.CIFailed
	case "running":
		return forge.CIRunning
	case "canceled", "cancelled", "canceling":
		return forge.CICancelled
	case "skipped", "manual":
		return forge.CISkipped
	case "pending", "created", "preparing", "scheduled", "waiting_for_resource":
		return forge.CIPending
	default:
		return forge.CIUnknown
	}
}

// aggregateCIState folds a set of normalized run states into a single
// aggregate: any failure → failed; any in-flight (running/pending) → that
// state; all success → success; otherwise unknown. Mirrors the
// "worst-wins, then in-flight, then success" convention.
func aggregateCIState(runs []forge.CIRun) string {
	if len(runs) == 0 {
		return forge.CIUnknown
	}
	var anyRunning, anyPending, anySuccess bool
	for _, r := range runs {
		switch r.Status {
		case forge.CIFailed:
			return forge.CIFailed
		case forge.CIRunning:
			anyRunning = true
		case forge.CIPending:
			anyPending = true
		case forge.CISuccess:
			anySuccess = true
		}
	}
	switch {
	case anyRunning:
		return forge.CIRunning
	case anyPending:
		return forge.CIPending
	case anySuccess:
		return forge.CISuccess
	default:
		// Only cancelled/skipped runs — no meaningful aggregate signal.
		return forge.CIUnknown
	}
}

// GetCIStatus returns the current aggregate CI state + per-job runs for a ref
// (a commit SHA or branch name). It reads GitLab's commit-statuses endpoint,
// which exposes one entry per CI job.
func (c *AdminClient) GetCIStatus(ctx context.Context, repo, ref string) (forge.CIStatus, error) {
	var statuses []gitlabCommitStatus
	code, err := c.do(ctx, http.MethodGet,
		"/projects/"+projectID(repo)+"/repository/commits/"+url.PathEscape(ref)+"/statuses", nil, &statuses)
	if err != nil {
		return forge.CIStatus{}, err
	}
	if code != http.StatusOK {
		return forge.CIStatus{}, statusErr("get ci status", code)
	}
	runs := make([]forge.CIRun, 0, len(statuses))
	sha := ref
	for _, s := range statuses {
		if s.SHA != "" {
			sha = s.SHA
		}
		runs = append(runs, forge.CIRun{
			Name:       s.Name,
			Status:     normCIStatus(s.Status),
			URL:        s.TargetURL,
			SHA:        s.SHA,
			StartedAt:  s.StartedAt,
			FinishedAt: s.FinishedAt,
		})
	}
	return forge.CIStatus{
		SHA:   sha,
		State: aggregateCIState(runs),
		Runs:  runs,
	}, nil
}

// gitlabPipeline is one pipeline entry (the GitLab /pipelines list shape).
type gitlabPipeline struct {
	ID        int       `json:"id"`
	Status    string    `json:"status"`
	Ref       string    `json:"ref"`
	SHA       string    `json:"sha"`
	WebURL    string    `json:"web_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ListCIHistory returns recent pipelines for a ref/branch, newest first, one
// CIRun per pipeline (Name="pipeline #<id>"). limit ≤ 0 → GitLab default page.
func (c *AdminClient) ListCIHistory(ctx context.Context, repo, ref string, limit int) ([]forge.CIRun, error) {
	vals := url.Values{}
	if ref != "" {
		vals.Set("ref", ref)
	}
	vals.Set("order_by", "id")
	vals.Set("sort", "desc")
	if limit > 0 {
		vals.Set("per_page", strconv.Itoa(limit))
	}
	var pipelines []gitlabPipeline
	code, err := c.do(ctx, http.MethodGet, "/projects/"+projectID(repo)+"/pipelines?"+vals.Encode(), nil, &pipelines)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, statusErr("list ci history", code)
	}
	out := make([]forge.CIRun, 0, len(pipelines))
	for _, p := range pipelines {
		out = append(out, forge.CIRun{
			Name:       "pipeline #" + strconv.Itoa(p.ID),
			Status:     normCIStatus(p.Status),
			URL:        p.WebURL,
			SHA:        p.SHA,
			StartedAt:  p.CreatedAt,
			FinishedAt: p.UpdatedAt,
		})
	}
	return out, nil
}

// CreatePull opens a merge request. SourceBranch/TargetBranch are GitLab's
// source_branch/target_branch; Draft is expressed via GitLab's "Draft:" title
// convention (there is no draft flag on the create call).
func (c *AdminClient) CreatePull(ctx context.Context, repo string, in forge.NewPull) (forge.PullRef, error) {
	title := in.Title
	if in.Draft && !strings.HasPrefix(strings.ToLower(title), "draft:") {
		title = "Draft: " + title
	}
	body := map[string]any{
		"source_branch": in.SourceBranch,
		"target_branch": in.TargetBranch,
		"title":         title,
	}
	if in.Body != "" {
		body["description"] = in.Body
	}
	var mr gitlabMR
	code, err := c.do(ctx, http.MethodPost, "/projects/"+projectID(repo)+"/merge_requests", body, &mr)
	if err != nil {
		return forge.PullRef{}, err
	}
	if code/100 != 2 {
		return forge.PullRef{}, statusErr("create merge request", code)
	}
	return mr.toRef(repo), nil
}

// UpdatePull applies a partial update. State transitions map onto GitLab's
// `state_event` (close|reopen); title/description/target_branch pass through.
func (c *AdminClient) UpdatePull(ctx context.Context, repo string, number int, patch forge.PullPatch) (forge.PullRef, error) {
	body := map[string]any{}
	if patch.Title != nil {
		body["title"] = *patch.Title
	}
	if patch.Body != nil {
		body["description"] = *patch.Body
	}
	if patch.TargetBranch != nil {
		body["target_branch"] = *patch.TargetBranch
	}
	if patch.State != nil {
		switch strings.ToLower(strings.TrimSpace(*patch.State)) {
		case "closed":
			body["state_event"] = "close"
		case "open", "opened":
			body["state_event"] = "reopen"
		}
	}
	var mr gitlabMR
	code, err := c.do(ctx, http.MethodPut, "/projects/"+projectID(repo)+"/merge_requests/"+strconv.Itoa(number), body, &mr)
	if err != nil {
		return forge.PullRef{}, err
	}
	if code/100 != 2 {
		return forge.PullRef{}, statusErr("update merge request", code)
	}
	return mr.toRef(repo), nil
}

// MergePull merges a merge request via PUT /merge_requests/{iid}/merge, which
// returns the merged MR. The "squash" method maps to GitLab's squash flag; the
// merge endpoint has no rebase variant (GitLab exposes rebase as a separate
// pre-merge step), so MergeRebase falls back to a standard merge.
func (c *AdminClient) MergePull(ctx context.Context, repo string, number int, opts forge.MergeOptions) (forge.PullRef, error) {
	body := map[string]any{}
	if opts.Method == forge.MergeSquash {
		body["squash"] = true
		if opts.CommitMessage != "" {
			body["squash_commit_message"] = opts.CommitMessage
		}
	} else if opts.CommitMessage != "" {
		body["merge_commit_message"] = opts.CommitMessage
	}
	if opts.DeleteBranch {
		body["should_remove_source_branch"] = true
	}
	if opts.SHA != "" {
		body["sha"] = opts.SHA
	}
	var mr gitlabMR
	code, err := c.do(ctx, http.MethodPut, "/projects/"+projectID(repo)+"/merge_requests/"+strconv.Itoa(number)+"/merge", body, &mr)
	if err != nil {
		return forge.PullRef{}, err
	}
	if code/100 != 2 {
		return forge.PullRef{}, statusErr("merge merge request", code)
	}
	return mr.toRef(repo), nil
}

var _ forge.PullClient = (*AdminClient)(nil)
