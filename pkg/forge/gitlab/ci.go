package gitlab

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// gitlabMR is the GitLab merge-request shape (read). Like issues, an MR is
// addressed by its per-project `iid`; `state` is "opened"/"merged"/"closed",
// `sha` is the head commit, `work_in_progress`/`draft` flag a draft.
// `source_project_id`/`target_project_id` name the projects the head and
// base branches live in — ids only; the MR object never carries the source
// project's path, which headProjectFor resolves.
type gitlabMR struct {
	IID             int        `json:"iid"`
	Title           string     `json:"title"`
	Description     string     `json:"description"`
	State           string     `json:"state"`
	WebURL          string     `json:"web_url"`
	SourceBranch    string     `json:"source_branch"`
	TargetBranch    string     `json:"target_branch"`
	SourceProjectID int64      `json:"source_project_id"`
	TargetProjectID int64      `json:"target_project_id"`
	SHA             string     `json:"sha"`
	Draft           bool       `json:"draft"`
	WorkInProgress  bool       `json:"work_in_progress"`
	Author          gitlabUser `json:"author"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// headProject is where a merge request's head branch lives, as far as it is
// proven: the project's path, and — for a fork — its own clone URL. The zero
// value is UNPROVEN, which every same-project lane reads as a refusal.
type headProject struct {
	path     string
	cloneURL string
}

// sourceProjectTTL bounds how long a resolved source project is reused: the
// answer changes only when the project is renamed or transferred.
const sourceProjectTTL = time.Hour

// sourceProjects caches resolved source projects per (instance, project id)
// across clients — a bearer client is built per call, so a cache on the
// client would never hit — so a fork costs one lookup an hour, not one per
// MR read.
var sourceProjects = struct {
	mu sync.Mutex
	m  map[string]sourceProjectEntry
}{m: map[string]sourceProjectEntry{}}

type sourceProjectEntry struct {
	headProject
	at time.Time
}

// headProjectFor resolves where a merge request's head branch lives.
// project is the reference the caller addressed the MR under — an MR lives
// in its TARGET project. A same-project MR (source and target ids agree) is
// that same reference, no round trip: a SameRepoAs against the caller's own
// reference holds by construction, whichever form (path or numeric id) the
// caller addressed the project by. A fork MR (differing ids) is looked up
// by its source project id — GET /projects/:id gives the path and clone URL
// the MR object never carries — so the same-project guards compare real
// names and a refusal can name the fork.
//
// Two shapes stay UNPROVEN on purpose rather than failing the read: an MR
// object without the ids, and a source project the token cannot see (404 /
// 403 — a private or deleted fork). Every same-project lane refuses an
// unproven head, which is the right answer for a fork it cannot inspect,
// and the MR's own fields still serve the lanes that only need its branches.
func (c *AdminClient) headProjectFor(ctx context.Context, project string, mr gitlabMR) (headProject, error) {
	switch {
	case mr.SourceProjectID <= 0 || mr.TargetProjectID <= 0:
		return headProject{}, nil
	case mr.SourceProjectID == mr.TargetProjectID:
		return headProject{path: strings.TrimSpace(project)}, nil
	}
	key := c.BaseURL + "#" + strconv.FormatInt(mr.SourceProjectID, 10)
	sourceProjects.mu.Lock()
	cached, ok := sourceProjects.m[key]
	sourceProjects.mu.Unlock()
	if ok && time.Since(cached.at) < sourceProjectTTL {
		return cached.headProject, nil
	}
	var out struct {
		PathWithNamespace string `json:"path_with_namespace"`
		HTTPURLToRepo     string `json:"http_url_to_repo"`
	}
	code, err := c.do(ctx, http.MethodGet, "/projects/"+strconv.FormatInt(mr.SourceProjectID, 10), nil, &out)
	if err != nil {
		return headProject{}, err
	}
	switch {
	case code == http.StatusNotFound || code == http.StatusForbidden:
		return headProject{}, nil
	case code != http.StatusOK:
		return headProject{}, statusErr("get source project", code)
	}
	head := headProject{path: strings.TrimSpace(out.PathWithNamespace), cloneURL: strings.TrimSpace(out.HTTPURLToRepo)}
	if head.path == "" {
		return headProject{}, nil
	}
	sourceProjects.mu.Lock()
	sourceProjects.m[key] = sourceProjectEntry{headProject: head, at: time.Now()}
	sourceProjects.mu.Unlock()
	return head, nil
}

// toRef normalizes a GitLab MR onto forge.PullRef. head is where its head
// branch was proven to live (headProjectFor); state "opened"→"open",
// "merged"/"closed" pass through; draft is the OR of the two GitLab flags.
func (mr gitlabMR) toRef(head headProject) forge.PullRef {
	return forge.PullRef{
		Number:           mr.IID,
		Title:            mr.Title,
		State:            normMRState(mr.State),
		URL:              mr.WebURL,
		SourceBranch:     mr.SourceBranch,
		TargetBranch:     mr.TargetBranch,
		HeadSHA:          mr.SHA,
		HeadRepoFullName: head.path,
		HeadCloneURL:     head.cloneURL,
		Author:           mr.Author.Username,
		Draft:            mr.Draft || mr.WorkInProgress,
		CreatedAt:        mr.CreatedAt,
		UpdatedAt:        mr.UpdatedAt,
		// GitLab has no first-class field for arbitrary issue linkage in the
		// MR payload, so LinkedIssues is parsed best-effort from title+body.
		LinkedIssues: forge.ParseIssueRefs(false, mr.Title, mr.Description),
	}
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
		head, err := c.headProjectFor(ctx, repo, mr)
		if err != nil {
			return nil, err
		}
		out = append(out, mr.toRef(head))
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
	head, err := c.headProjectFor(ctx, repo, mr)
	if err != nil {
		return forge.PullRef{}, err
	}
	return mr.toRef(head), nil
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
	head, err := c.headProjectFor(ctx, repo, mr)
	if err != nil {
		return forge.PullRef{}, err
	}
	return mr.toRef(head), nil
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
	head, err := c.headProjectFor(ctx, repo, mr)
	if err != nil {
		return forge.PullRef{}, err
	}
	return mr.toRef(head), nil
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
	head, err := c.headProjectFor(ctx, repo, mr)
	if err != nil {
		return forge.PullRef{}, err
	}
	return mr.toRef(head), nil
}

var _ forge.PullClient = (*AdminClient)(nil)
