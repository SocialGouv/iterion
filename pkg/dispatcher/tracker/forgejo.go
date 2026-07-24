package tracker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// ForgejoOptions configures the Forgejo (Gitea-compatible) adapter.
type ForgejoOptions struct {
	Host  string // base URL, e.g. "https://codeberg.org"
	Repo  string // "owner/repo"
	Token string // FORGEJO_TOKEN — sent as Authorization header

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

// ForgejoAdapter implements Tracker against the Forgejo/Gitea API. It
// uses direct REST calls because there is no widely-installed CLI
// equivalent of `gh` for Forgejo.
type ForgejoAdapter struct {
	opts ForgejoOptions
	rc   *restClient

	labelMu  sync.RWMutex
	labelIDs map[string]int64 // name → ID cache, lazily populated on first Claim/Release
}

// NewForgejo returns a configured adapter.
func NewForgejo(opts ForgejoOptions) (*ForgejoAdapter, error) {
	if opts.Host == "" {
		return nil, errors.New("forgejo tracker: host is required")
	}
	if err := ValidateRepoPath(opts.Repo); err != nil {
		return nil, fmt.Errorf("forgejo tracker: %w", err)
	}
	opts.ClaimedLabel = defaultClaimedLabel(opts.ClaimedLabel)
	hc := defaultHTTPTrackerClient(opts.HTTPClient)
	opts.Host = strings.TrimRight(opts.Host, "/")
	rc := &restClient{
		baseURL:     opts.Host + "/api/v1",
		hc:          hc,
		errPrefix:   "forgejo",
		conflictErr: errForgejoConflict,
		setAuth: func(req *http.Request) {
			if opts.Token != "" {
				req.Header.Set("Authorization", "token "+opts.Token)
			}
		},
	}
	return &ForgejoAdapter{opts: opts, rc: rc}, nil
}

// Name implements Tracker.
func (a *ForgejoAdapter) Name() string { return "forgejo" }

// ListCandidates returns open issues matching the include/exclude
// label filters (filter executed locally — Forgejo doesn't have
// negative-label search in the same syntax as GitHub).
func (a *ForgejoAdapter) ListCandidates(ctx context.Context) ([]Issue, error) {
	const pageSize = 50
	var pending []Issue
	openNums := map[int]bool{} // all open issues seen — fail-open blocker set
	err := paginateUntilShort(pageSize, 100, a.opts.Logger,
		fmt.Sprintf("forgejo tracker: ListCandidates hit the 100-page cap on repo %s — beyond this point issues are silently dropped from dispatch; consider tightening label filters", a.opts.Repo),
		func(page int) (int, error) {
			q := url.Values{}
			q.Set("state", "open")
			q.Set("type", "issues")
			q.Set("limit", fmt.Sprintf("%d", pageSize))
			q.Set("page", fmt.Sprintf("%d", page))
			if len(a.opts.IncludeLabels) > 0 {
				q.Set("labels", strings.Join(a.opts.IncludeLabels, ","))
			}
			endpoint := fmt.Sprintf("/repos/%s/issues?%s", a.opts.Repo, q.Encode())

			var raw []forgejoIssue
			if err := a.do(ctx, http.MethodGet, endpoint, nil, &raw); err != nil {
				return 0, err
			}
			for _, r := range raw {
				openNums[r.Number] = true
				labels := labelNames(r.Labels)
				if anyOfString(labels, a.opts.ExcludeLabels) {
					continue
				}
				if slices.Contains(labels, a.opts.ClaimedLabel) {
					continue
				}
				iss := a.toIssue(r)
				if iss.WorkflowState == "" {
					continue
				}
				pending = append(pending, iss)
			}
			return len(raw), nil
		})
	if err != nil {
		return nil, err
	}
	// Second pass (after every page is in openNums): hold any issue whose
	// body declares a still-open blocker. Fail-open — see blockers.go.
	return filterHeldByBlockers(pending, openNums, a.opts.Logger, "forgejo"), nil
}

// RefreshStates fetches each issue and re-derives its state from
// current labels.
func (a *ForgejoAdapter) RefreshStates(ctx context.Context, ids []string) (map[string]string, error) {
	fetchState := func(num int) (string, error) {
		var r forgejoIssue
		if err := a.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d", a.opts.Repo, num), nil, &r); err != nil {
			return "", err
		}
		return a.toIssue(r).WorkflowState, nil
	}
	parseID := func(id string) (int, bool) { return parseForgejoID(a.opts.Host, a.opts.Repo, id) }
	return refreshStateByID(ids, parseID, fetchState, a.opts.Logger, "dispatcher: forgejo RefreshStates"), nil
}

// UpdateState replaces an issue's labels with the include set from the
// matching state mapping. Returns ErrTransitionRejected if newState
// has no mapping.
func (a *ForgejoAdapter) UpdateState(ctx context.Context, id, newState string) error {
	sel, err := resolveLabelSelector(a.opts.StateMapping, newState)
	if err != nil {
		return err
	}
	num, ok := parseForgejoID(a.opts.Host, a.opts.Repo, id)
	if !ok {
		return ErrNotFound
	}
	// Read current labels, apply the diff: drop excludes from sel,
	// add includes from sel. Keeps unrelated labels untouched.
	var current forgejoIssue
	if err := a.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d", a.opts.Repo, num), nil, &current); err != nil {
		return err
	}
	have := applyLabelDiff(labelNames(current.Labels), sel)
	return a.replaceLabels(ctx, num, have)
}

// Comment adds a comment to the issue.
func (a *ForgejoAdapter) Comment(ctx context.Context, id, body string) error {
	num, ok := parseForgejoID(a.opts.Host, a.opts.Repo, id)
	if !ok {
		return ErrNotFound
	}
	in := map[string]string{"body": body}
	return a.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", a.opts.Repo, num), in, nil)
}

// Claim adds ClaimedLabel via a single POST /issues/{n}/labels call.
// The previous implementation issued a GET-then-PUT round trip; the
// add endpoint expresses the intent directly and is half the HTTP
// cost. We resolve the label name to its numeric ID on first use and
// cache the mapping; if the label doesn't exist yet on the repo we
// create it so a fresh repository can be dispatched without manual
// label setup.
func (a *ForgejoAdapter) Claim(ctx context.Context, id, marker string) error {
	num, ok := parseForgejoID(a.opts.Host, a.opts.Repo, id)
	if !ok {
		return ErrNotFound
	}
	lid, err := a.resolveLabelID(ctx, a.opts.ClaimedLabel)
	if err != nil {
		return err
	}
	body := struct {
		Labels []int64 `json:"labels"`
	}{Labels: []int64{lid}}
	return a.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/labels", a.opts.Repo, num), body, nil)
}

// Release removes ClaimedLabel via DELETE /issues/{n}/labels/{labelID}.
// Idempotent: 404 from Forgejo (label not present on the issue) is
// folded into success.
func (a *ForgejoAdapter) Release(ctx context.Context, id, marker string) error {
	num, ok := parseForgejoID(a.opts.Host, a.opts.Repo, id)
	if !ok {
		return ErrNotFound
	}
	lid, err := a.resolveLabelID(ctx, a.opts.ClaimedLabel)
	if err != nil {
		return err
	}
	err = a.do(ctx, http.MethodDelete, fmt.Sprintf("/repos/%s/issues/%d/labels/%d", a.opts.Repo, num, lid), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// resolveLabelID looks up a label by name on the configured repo and
// returns its numeric ID. Results are cached for the lifetime of the
// adapter. If the label doesn't exist yet, it is created.
func (a *ForgejoAdapter) resolveLabelID(ctx context.Context, name string) (int64, error) {
	a.labelMu.RLock()
	if id, ok := a.labelIDs[name]; ok {
		a.labelMu.RUnlock()
		return id, nil
	}
	a.labelMu.RUnlock()

	a.labelMu.Lock()
	defer a.labelMu.Unlock()
	if id, ok := a.labelIDs[name]; ok {
		return id, nil
	}
	if a.labelIDs == nil {
		a.labelIDs = map[string]int64{}
	}

	var labels []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := a.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/labels?limit=200", a.opts.Repo), nil, &labels); err != nil {
		return 0, err
	}
	for _, l := range labels {
		a.labelIDs[l.Name] = l.ID
	}
	if id, ok := a.labelIDs[name]; ok {
		return id, nil
	}

	// Label missing on the repo — create it. Color is required by the
	// API; pick a neutral grey so iterion's labels don't visually clash
	// with the user's palette.
	in := map[string]any{"name": name, "color": "#888888"}
	var created struct {
		ID int64 `json:"id"`
	}
	err := a.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/labels", a.opts.Repo), in, &created)
	if err == nil {
		a.labelIDs[name] = created.ID
		return created.ID, nil
	}
	// 409 means another dispatcher process won the race and created the
	// label between our GET above and this POST. Re-fetch and use
	// theirs instead of returning the conflict to the caller.
	if errors.Is(err, errForgejoConflict) {
		var labels2 []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		}
		if rerr := a.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/labels?limit=200", a.opts.Repo), nil, &labels2); rerr != nil {
			return 0, fmt.Errorf("forgejo: re-list labels after 409: %w", rerr)
		}
		for _, l := range labels2 {
			a.labelIDs[l.Name] = l.ID
		}
		if id, ok := a.labelIDs[name]; ok {
			return id, nil
		}
		return 0, fmt.Errorf("forgejo: label %q reported as conflict but not visible on re-list", name)
	}
	return 0, err
}

// ---------------------------------------------------------------------------
// internals
// ---------------------------------------------------------------------------

type forgejoIssue struct {
	Number    int            `json:"number"`
	Title     string         `json:"title"`
	Body      string         `json:"body"`
	State     string         `json:"state"`
	Labels    []forgejoLabel `json:"labels"`
	Assignees []forgejoUser  `json:"assignees"`
	User      forgejoUser    `json:"user"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	URL       string         `json:"html_url"`
}

type forgejoLabel struct {
	Name string `json:"name"`
}

type forgejoUser struct {
	Login string `json:"login"`
}

func (a *ForgejoAdapter) toIssue(r forgejoIssue) Issue {
	labels := filterOutString(labelNames(r.Labels), a.opts.ClaimedLabel)
	id := fmt.Sprintf("forgejo:%s/%s#%d", trimHost(a.opts.Host), a.opts.Repo, r.Number)
	assignee := ""
	if len(r.Assignees) > 0 {
		assignee = r.Assignees[0].Login
	}
	return Issue{
		ID:            id,
		Identifier:    fmt.Sprintf("%s#%d", a.opts.Repo, r.Number),
		Title:         r.Title,
		Body:          r.Body,
		WorkflowState: resolveStateByLabels(labels, a.opts.StateMapping),
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
		Labels:        labels,
		Assignee:      assignee,
		Metadata: map[string]string{
			"url":           r.URL,
			"forgejo_state": r.State,
			"author":        r.User.Login,
		},
	}
}

func (a *ForgejoAdapter) replaceLabels(ctx context.Context, num int, labels []string) error {
	in := struct {
		Labels []string `json:"labels"`
	}{Labels: labels}
	return a.do(ctx, http.MethodPut, fmt.Sprintf("/repos/%s/issues/%d/labels", a.opts.Repo, num), in, nil)
}

// do performs an authenticated request against the Forgejo API. See
// restClient.do for the shared request/error-mapping behavior.
func (a *ForgejoAdapter) do(ctx context.Context, method, path string, in any, out any) error {
	return a.rc.do(ctx, method, path, in, out)
}

// errForgejoConflict signals a 409 from a Forgejo write — typically a
// "label already exists" when two dispatcher processes race to create
// the same `iterion-claimed` label on first Claim. Callers that own a
// fallback path (refresh the GET, then proceed) recognise it via
// errors.Is.
var errForgejoConflict = errors.New("forgejo: conflict (409)")

func labelNames(in []forgejoLabel) []string {
	out := make([]string, 0, len(in))
	for _, l := range in {
		out = append(out, l.Name)
	}
	return out
}

func trimHost(h string) string {
	h = strings.TrimPrefix(h, "https://")
	h = strings.TrimPrefix(h, "http://")
	// Lowercase: RFC 3986 hosts are case-insensitive, and this value is used
	// both to BUILD an issue ID and to PARSE it back — a case mismatch across
	// processes would silently lose claim/state tracking.
	return strings.ToLower(strings.TrimRight(h, "/"))
}

// parseForgejoID expects "forgejo:<host>/<owner>/<repo>#<num>".
func parseForgejoID(host, repo, id string) (int, bool) {
	return parsePrefixedID("forgejo:"+trimHost(host)+"/"+repo+"#", id)
}
