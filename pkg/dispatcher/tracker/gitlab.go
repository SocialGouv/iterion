package tracker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// Compile-time assertion that GitLabAdapter satisfies the contract.
var _ Tracker = (*GitLabAdapter)(nil)

// GitLabOptions configures the GitLab adapter.
type GitLabOptions struct {
	Host  string // base URL, e.g. "https://gitlab.com"
	Repo  string // "owner/repo" or "group/subgroup/repo"
	Token string // GITLAB_TOKEN — sent as the PRIVATE-TOKEN header

	IncludeLabels []string
	ExcludeLabels []string
	StateMapping  map[string]LabelSelector
	ClaimedLabel  string

	// HTTPClient overrides the default net/http client. Useful for
	// tests; production leaves it nil.
	HTTPClient *http.Client

	// Logger lets the adapter surface per-issue HTTP errors during
	// state refresh without failing the whole sweep. nil disables
	// such warnings (legacy behaviour).
	Logger *iterlog.Logger
}

// GitLabAdapter implements Tracker against the GitLab v4 REST API. It
// uses direct REST calls — GitLab addresses a project by its
// URL-encoded full path, and labels are referenced by name (no numeric
// ID resolution needed, unlike Forgejo).
type GitLabAdapter struct {
	opts GitLabOptions
	rc   *restClient

	// pid is the URL-encoded project path used in every endpoint. Since
	// ValidateRepoPath restricts repo to [A-Za-z0-9._-] segments joined
	// by '/', the only character needing encoding is the slash.
	pid string
}

// NewGitLab returns a configured adapter.
func NewGitLab(opts GitLabOptions) (*GitLabAdapter, error) {
	if opts.Host == "" {
		return nil, errors.New("gitlab tracker: host is required")
	}
	if err := ValidateRepoPath(opts.Repo); err != nil {
		return nil, fmt.Errorf("gitlab tracker: %w", err)
	}
	opts.ClaimedLabel = defaultClaimedLabel(opts.ClaimedLabel)
	hc := defaultHTTPTrackerClient(opts.HTTPClient)
	opts.Host = strings.TrimRight(opts.Host, "/")
	pid := strings.ReplaceAll(opts.Repo, "/", "%2F")
	rc := &restClient{
		baseURL:   opts.Host + "/api/v4",
		hc:        hc,
		errPrefix: "gitlab",
		setAuth: func(req *http.Request) {
			if opts.Token != "" {
				req.Header.Set("PRIVATE-TOKEN", opts.Token)
			}
		},
	}
	return &GitLabAdapter{opts: opts, rc: rc, pid: pid}, nil
}

// Name implements Tracker.
func (a *GitLabAdapter) Name() string { return "gitlab" }

// ListCandidates returns open issues matching the include/exclude label
// filters. The include filter is pushed to the server (GitLab supports
// AND-label search via the `labels` query param); the exclude filter and
// claimed-skip run locally.
func (a *GitLabAdapter) ListCandidates(ctx context.Context) ([]Issue, error) {
	const pageSize = 50
	out := make([]Issue, 0)
	for page := 1; ; page++ {
		q := url.Values{}
		q.Set("state", "opened")
		q.Set("per_page", fmt.Sprintf("%d", pageSize))
		q.Set("page", fmt.Sprintf("%d", page))
		if len(a.opts.IncludeLabels) > 0 {
			q.Set("labels", strings.Join(a.opts.IncludeLabels, ","))
		}
		endpoint := fmt.Sprintf("/projects/%s/issues?%s", a.pid, q.Encode())

		var raw []gitlabIssue
		if err := a.do(ctx, http.MethodGet, endpoint, nil, &raw); err != nil {
			return nil, err
		}
		for _, r := range raw {
			if anyOfString(r.Labels, a.opts.ExcludeLabels) {
				continue
			}
			if slices.Contains(r.Labels, a.opts.ClaimedLabel) {
				continue
			}
			iss := a.toIssue(r)
			if iss.WorkflowState == "" {
				continue
			}
			out = append(out, iss)
		}
		// Stop when the page is short or empty — the cheapest portable
		// signal that there is no further page.
		if len(raw) < pageSize {
			break
		}
		// Belt + suspenders: cap total pages to avoid runaway loops on
		// pathological responses (e.g. a buggy server returning pageSize
		// entries forever).
		if page >= 100 {
			if a.opts.Logger != nil {
				a.opts.Logger.Warn("gitlab tracker: ListCandidates hit the 100-page cap on repo %s — beyond this point issues are silently dropped from dispatch; consider tightening label filters", a.opts.Repo)
			}
			break
		}
	}
	return out, nil
}

// RefreshStates fetches each issue and re-derives its state from current
// labels.
func (a *GitLabAdapter) RefreshStates(ctx context.Context, ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		num, ok := parseGitLabID(a.opts.Host, a.opts.Repo, id)
		if !ok {
			continue
		}
		var r gitlabIssue
		if err := a.do(ctx, http.MethodGet, fmt.Sprintf("/projects/%s/issues/%d", a.pid, num), nil, &r); err != nil {
			// Log + skip on per-issue errors so a transient network blip
			// on one issue doesn't make the dispatcher treat the rest as
			// "disappeared from tracker" (which would cancel their
			// in-flight runs).
			if a.opts.Logger != nil {
				a.opts.Logger.Warn("dispatcher: gitlab RefreshStates: issue %d: %v", num, err)
			}
			continue
		}
		iss := a.toIssue(r)
		if iss.WorkflowState != "" {
			out[id] = iss.WorkflowState
		}
	}
	return out, nil
}

// UpdateState replaces an issue's labels with the include set from the
// matching state mapping. Returns ErrTransitionRejected if newState has
// no mapping.
func (a *GitLabAdapter) UpdateState(ctx context.Context, id, newState string) error {
	sel, ok := a.opts.StateMapping[newState]
	if !ok {
		return fmt.Errorf("%w: no label mapping for state %q", ErrTransitionRejected, newState)
	}
	num, ok := parseGitLabID(a.opts.Host, a.opts.Repo, id)
	if !ok {
		return ErrNotFound
	}
	// Read current labels, apply the diff: drop excludes from sel, add
	// includes from sel. Keeps unrelated labels untouched.
	var current gitlabIssue
	if err := a.do(ctx, http.MethodGet, fmt.Sprintf("/projects/%s/issues/%d", a.pid, num), nil, &current); err != nil {
		return err
	}
	have := applyLabelDiff(slices.Clone(current.Labels), sel)
	// GitLab replaces the full label set when `labels` is sent (comma-joined).
	in := map[string]string{"labels": strings.Join(have, ",")}
	return a.do(ctx, http.MethodPut, fmt.Sprintf("/projects/%s/issues/%d", a.pid, num), in, nil)
}

// Comment adds a note to the issue.
func (a *GitLabAdapter) Comment(ctx context.Context, id, body string) error {
	num, ok := parseGitLabID(a.opts.Host, a.opts.Repo, id)
	if !ok {
		return ErrNotFound
	}
	in := map[string]string{"body": body}
	return a.do(ctx, http.MethodPost, fmt.Sprintf("/projects/%s/issues/%d/notes", a.pid, num), in, nil)
}

// Claim adds ClaimedLabel via PUT with add_labels. GitLab uses label
// names directly — no numeric-ID resolution and the label is created
// on the fly if it doesn't exist.
func (a *GitLabAdapter) Claim(ctx context.Context, id, marker string) error {
	num, ok := parseGitLabID(a.opts.Host, a.opts.Repo, id)
	if !ok {
		return ErrNotFound
	}
	in := map[string]string{"add_labels": a.opts.ClaimedLabel}
	return a.do(ctx, http.MethodPut, fmt.Sprintf("/projects/%s/issues/%d", a.pid, num), in, nil)
}

// Release removes ClaimedLabel via PUT with remove_labels. Idempotent:
// removing an absent label is not an error on GitLab, and any 404 is
// folded into success.
func (a *GitLabAdapter) Release(ctx context.Context, id, marker string) error {
	num, ok := parseGitLabID(a.opts.Host, a.opts.Repo, id)
	if !ok {
		return ErrNotFound
	}
	in := map[string]string{"remove_labels": a.opts.ClaimedLabel}
	err := a.do(ctx, http.MethodPut, fmt.Sprintf("/projects/%s/issues/%d", a.pid, num), in, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// ---------------------------------------------------------------------------
// internals
// ---------------------------------------------------------------------------

type gitlabIssue struct {
	IID         int          `json:"iid"`
	Title       string       `json:"title"`
	Description string       `json:"description"`
	State       string       `json:"state"`
	Labels      []string     `json:"labels"`
	Author      gitlabUser   `json:"author"`
	Assignees   []gitlabUser `json:"assignees"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
	WebURL      string       `json:"web_url"`
}

type gitlabUser struct {
	Username string `json:"username"`
}

func (a *GitLabAdapter) toIssue(r gitlabIssue) Issue {
	labels := filterOutString(r.Labels, a.opts.ClaimedLabel)
	id := fmt.Sprintf("gitlab:%s/%s#%d", trimHost(a.opts.Host), a.opts.Repo, r.IID)
	assignee := ""
	if len(r.Assignees) > 0 {
		assignee = r.Assignees[0].Username
	}
	return Issue{
		ID:            id,
		Identifier:    fmt.Sprintf("%s#%d", a.opts.Repo, r.IID),
		Title:         r.Title,
		Body:          r.Description,
		WorkflowState: a.resolveState(labels),
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
		Labels:        labels,
		Assignee:      assignee,
		Metadata: map[string]string{
			"url":          r.WebURL,
			"gitlab_state": r.State,
			"author":       r.Author.Username,
		},
	}
}

func (a *GitLabAdapter) resolveState(labels []string) string {
	return resolveStateByLabels(labels, a.opts.StateMapping)
}

// do performs an authenticated request against the GitLab v4 API. See
// restClient.do for the shared request/error-mapping behavior.
func (a *GitLabAdapter) do(ctx context.Context, method, path string, in any, out any) error {
	return a.rc.do(ctx, method, path, in, out)
}

// parseGitLabID expects "gitlab:<host>/<owner>/<repo>#<iid>".
func parseGitLabID(host, repo, id string) (int, bool) {
	return parsePrefixedID("gitlab:"+trimHost(host)+"/"+repo+"#", id)
}
