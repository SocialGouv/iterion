package forgejo

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// PullClient: Forgejo/Gitea pull-request + CI capability surfacing linked PRs
// and commit-status CI state (current + history) on board cards.
//
// CI on Forgejo/Gitea is exposed as commit statuses (and, on newer Forgejo,
// Actions). We use the commit-status surface — combined status for the
// aggregate (GetCIStatus) and per-context statuses for history
// (ListCIHistory) — which is portable across Gitea and every Forgejo version.
var _ forge.PullClient = (*AdminClient)(nil)

type forgejoBranchInfo struct {
	Ref  string `json:"ref"`
	Sha  string `json:"sha"`
	Repo *struct {
		FullName string `json:"full_name"`
		CloneURL string `json:"clone_url"`
	} `json:"repo,omitempty"`
}

// forgejoPull mirrors the Gitea API PullRequest shape (subset we normalize).
type forgejoPull struct {
	Number  int64              `json:"number"`
	Title   string             `json:"title"`
	Body    string             `json:"body"`
	State   string             `json:"state"` // "open" | "closed"
	HTMLURL string             `json:"html_url"`
	Draft   bool               `json:"draft"`
	Merged  bool               `json:"merged"`
	Poster  *forgejoUser       `json:"user"`
	Head    *forgejoBranchInfo `json:"head"`
	Base    *forgejoBranchInfo `json:"base"`
	Created *time.Time         `json:"created_at"`
	Updated *time.Time         `json:"updated_at"`
}

func (p forgejoPull) toRef() forge.PullRef {
	state := p.State
	if p.Merged {
		state = "merged"
	}
	ref := forge.PullRef{
		Number: int(p.Number),
		Title:  p.Title,
		State:  state,
		URL:    p.HTMLURL,
		Draft:  p.Draft,
	}
	if p.Poster != nil {
		ref.Author = p.Poster.Login
	}
	if p.Head != nil {
		ref.SourceBranch = p.Head.Ref
		ref.HeadSHA = p.Head.Sha
		if p.Head.Repo != nil {
			ref.HeadRepoFullName = p.Head.Repo.FullName
			ref.HeadCloneURL = p.Head.Repo.CloneURL
		}
	}
	if p.Base != nil {
		ref.TargetBranch = p.Base.Ref
	}
	if p.Created != nil {
		ref.CreatedAt = *p.Created
	}
	if p.Updated != nil {
		ref.UpdatedAt = *p.Updated
	}
	// A literal "#0" is discarded (skipNonPositive=true) — Forgejo/Gitea issue
	// numbers start at 1, so "#0" is never a real reference.
	ref.LinkedIssues = forge.ParseIssueRefs(true, p.Title, p.Body)
	return ref
}

// ListPullRequests lists PRs for a repo. state defaults to "open".
func (c *AdminClient) ListPullRequests(ctx context.Context, repo string, opts forge.PullListOptions) ([]forge.PullRef, error) {
	vals := url.Values{}
	state := opts.State
	if state == "" {
		state = "open"
	}
	vals.Set("state", state)
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

	var pulls []forgejoPull
	code, err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/pulls?"+vals.Encode(), nil, &pulls)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, statusErr("GET pulls", code)
	}
	out := make([]forge.PullRef, 0, len(pulls))
	for _, p := range pulls {
		out = append(out, p.toRef())
	}
	return out, nil
}

// GetPullRequest fetches one PR by index.
func (c *AdminClient) GetPullRequest(ctx context.Context, repo string, number int) (forge.PullRef, error) {
	var p forgejoPull
	code, err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/pulls/"+strconv.Itoa(number), nil, &p)
	if err != nil {
		return forge.PullRef{}, err
	}
	if code != http.StatusOK {
		return forge.PullRef{}, statusErr("GET pull", code)
	}
	return p.toRef(), nil
}

// forgejoCommitStatus is one per-context commit status. Note the JSON quirk:
// in the COMBINED status payload the per-context state is `status`, while the
// aggregate top-level state is `state`.
type forgejoCommitStatus struct {
	State       string    `json:"status"`
	Context     string    `json:"context"`
	Description string    `json:"description"`
	TargetURL   string    `json:"target_url"`
	URL         string    `json:"url"`
	Created     time.Time `json:"created_at"`
	Updated     time.Time `json:"updated_at"`
}

// forgejoCombinedStatus is GET /commits/{ref}/status.
type forgejoCombinedStatus struct {
	State    string                `json:"state"`
	SHA      string                `json:"sha"`
	Statuses []forgejoCommitStatus `json:"statuses"`
}

// mapCIState normalizes a Gitea commit-status state to a forge.CI* constant.
// failure/error → failed; warning → pending (advisory, non-terminal).
func mapCIState(s string) string {
	switch s {
	case "success":
		return forge.CISuccess
	case "pending":
		return forge.CIPending
	case "failure", "error":
		return forge.CIFailed
	case "warning":
		return forge.CIPending
	case "skipped":
		return forge.CISkipped
	default:
		return forge.CIUnknown
	}
}

func (s forgejoCommitStatus) toRun(sha string) forge.CIRun {
	return forge.CIRun{
		Name:       s.Context,
		Status:     mapCIState(s.State),
		Conclusion: s.State,
		URL:        s.TargetURL,
		SHA:        sha,
		StartedAt:  s.Created,
		FinishedAt: s.Updated,
	}
}

// GetCIStatus returns the current aggregate CI state + runs for a ref (commit
// SHA or branch HEAD) via the combined commit-status endpoint.
func (c *AdminClient) GetCIStatus(ctx context.Context, repo, ref string) (forge.CIStatus, error) {
	var cs forgejoCombinedStatus
	code, err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/commits/"+url.PathEscape(ref)+"/status", nil, &cs)
	if err != nil {
		return forge.CIStatus{}, err
	}
	if code != http.StatusOK {
		return forge.CIStatus{}, statusErr("GET commit status", code)
	}
	sha := cs.SHA
	if sha == "" {
		sha = ref
	}
	out := forge.CIStatus{
		SHA:   sha,
		State: mapCIState(cs.State),
	}
	for _, s := range cs.Statuses {
		out.Runs = append(out.Runs, s.toRun(sha))
	}
	return out, nil
}

// ListCIHistory returns recent CI runs for a ref/branch HEAD via the
// per-context statuses endpoint, newest first, up to limit (0 → 50).
//
// Best-effort: this is the commit-status surface, not Forgejo Actions tasks;
// statuses-based history is the portable contract across Gitea/Forgejo.
func (c *AdminClient) ListCIHistory(ctx context.Context, repo, ref string, limit int) ([]forge.CIRun, error) {
	if limit <= 0 {
		limit = 50
	}
	vals := url.Values{}
	vals.Set("limit", strconv.Itoa(limit))
	vals.Set("sort", "recentupdate")

	var statuses []forgejoCommitStatus
	code, err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/commits/"+url.PathEscape(ref)+"/statuses?"+vals.Encode(), nil, &statuses)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, statusErr("GET commit statuses", code)
	}
	out := make([]forge.CIRun, 0, len(statuses))
	for _, s := range statuses {
		out = append(out, s.toRun(ref))
	}
	return out, nil
}

// CreatePull opens a pull request. head/base are branch names. Gitea has no
// reliable draft-on-create flag, so NewPull.Draft is not honoured here (open
// then mark via title if needed).
func (c *AdminClient) CreatePull(ctx context.Context, repo string, in forge.NewPull) (forge.PullRef, error) {
	body := map[string]any{
		"title": in.Title,
		"head":  in.SourceBranch,
		"base":  in.TargetBranch,
	}
	if in.Body != "" {
		body["body"] = in.Body
	}
	var p forgejoPull
	code, err := c.do(ctx, http.MethodPost, "/repos/"+repo+"/pulls", body, &p)
	if err != nil {
		return forge.PullRef{}, err
	}
	if code/100 != 2 {
		return forge.PullRef{}, statusErr("create pull", code)
	}
	return p.toRef(), nil
}

// UpdatePull applies a partial update (title/body/base/state) via PATCH.
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
	var p forgejoPull
	code, err := c.do(ctx, http.MethodPatch, "/repos/"+repo+"/pulls/"+strconv.Itoa(number), body, &p)
	if err != nil {
		return forge.PullRef{}, err
	}
	if code/100 != 2 {
		return forge.PullRef{}, statusErr("update pull", code)
	}
	return p.toRef(), nil
}

// MergePull merges a PR via POST /pulls/{index}/merge (Gitea returns an empty
// 200), then re-fetches it so the returned ref reflects the merged state.
// delete_branch_after_merge handles branch deletion natively. Gitea's `Do`
// field shares GitHub's merge-method vocabulary (see forge.MergeMethodWire).
func (c *AdminClient) MergePull(ctx context.Context, repo string, number int, opts forge.MergeOptions) (forge.PullRef, error) {
	body := map[string]any{"Do": forge.MergeMethodWire(opts.Method)}
	if opts.CommitTitle != "" {
		body["MergeTitleField"] = opts.CommitTitle
	}
	if opts.CommitMessage != "" {
		body["MergeMessageField"] = opts.CommitMessage
	}
	if opts.DeleteBranch {
		body["delete_branch_after_merge"] = true
	}
	if opts.SHA != "" {
		body["head_commit_id"] = opts.SHA
	}
	code, err := c.do(ctx, http.MethodPost, "/repos/"+repo+"/pulls/"+strconv.Itoa(number)+"/merge", body, nil)
	if err != nil {
		return forge.PullRef{}, err
	}
	if code/100 != 2 {
		return forge.PullRef{}, statusErr("merge pull", code)
	}
	merged, err := c.GetPullRequest(ctx, repo, number)
	if err != nil {
		return forge.PullRef{State: "merged", Number: number}, nil
	}
	return merged, nil
}
