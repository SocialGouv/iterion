package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// GitHubOptions configures the GitHub Issues adapter. The Token field
// is optional — when empty, the adapter relies on `gh auth status`
// having a valid login already.
type GitHubOptions struct {
	// Repo is "owner/repo".
	Repo string

	// Token, when non-empty, is exported as GH_TOKEN to the gh
	// subprocess so the adapter works in non-interactive contexts
	// (CI). Empty means rely on the existing gh login.
	Token string

	// IncludeLabels, ExcludeLabels narrow the candidate pool. All
	// IncludeLabels must be present; any ExcludeLabel disqualifies.
	IncludeLabels []string
	ExcludeLabels []string

	// AuthorAllowlist, when non-empty, restricts candidates to issues opened by
	// one of these logins (case-insensitive) — the trusted-author scope for
	// auto-dispatch on a public repo. Empty = any author.
	AuthorAllowlist []string

	// StateMapping maps a workflow state name to a label predicate.
	// The first entry that matches in iteration order determines
	// the issue's WorkflowState. Map iteration order is unspecified
	// in Go, so callers should treat ordering as best-effort and
	// design label predicates so at most one matches per issue.
	//
	// Ignored in BOARD MODE (see Project): a bound project's Status
	// field is the state, and a parallel label convention would be a
	// second answer to the same question.
	StateMapping map[string]LabelSelector

	// Project, when non-nil, puts the adapter in BOARD MODE: the workflow
	// state is read from (and written to) a Projects v2 board's Status
	// field instead of labels (ADR-097). The claim stays a label either
	// way — a project item has nothing to fence a lease with.
	Project *GitHubProjectOptions

	// ClaimedLabel is added by Claim and removed by Release. Issues
	// carrying this label are filtered out of ListCandidates.
	// Defaults to "iterion-claimed".
	ClaimedLabel string

	// Command, when non-nil, overrides the gh subprocess factory.
	// Used by tests to inject fake responses. Production leaves it
	// nil so the adapter shells out to the real `gh`.
	Command func(ctx context.Context, args []string, env []string) ([]byte, error)

	// Logger, when non-nil, receives warnings about silent
	// degradations (e.g. ListCandidates hitting the per-poll cap).
	// Optional — adapter is fully functional without it.
	Logger *iterlog.Logger
}

// LabelSelector restricts a state mapping by label allowlist / blocklist.
type LabelSelector struct {
	LabelsInclude []string
	LabelsExclude []string
}

// GitHubAdapter implements Tracker over the GitHub Issues API by
// shelling out to the `gh` CLI. Auth, OAuth, rate-limit handling and
// pagination come for free from gh; iterion only deals with JSON.
type GitHubAdapter struct {
	opts GitHubOptions
}

// NewGitHub returns a configured adapter. Returns an error if the
// minimum config (repo) is missing.
func NewGitHub(opts GitHubOptions) (*GitHubAdapter, error) {
	if err := ValidateRepoPath(opts.Repo); err != nil {
		return nil, fmt.Errorf("github tracker: %w", err)
	}
	if opts.Project != nil {
		if err := opts.Project.validate(); err != nil {
			return nil, err
		}
	}
	opts.ClaimedLabel = defaultClaimedLabel(opts.ClaimedLabel)
	if opts.Command == nil {
		opts.Command = runGH
	}
	return &GitHubAdapter{opts: opts}, nil
}

// Name implements Tracker.
func (a *GitHubAdapter) Name() string { return "github" }

// ghCandidateListLimit caps the number of candidates we pull per poll.
// gh CLI paginates internally up to --limit, so a single invocation
// covers very large backlogs without us implementing pagination
// ourselves. Set high enough that an active repo never silently drops
// candidates; if a poll returns exactly this many we log a warning
// so the operator knows to investigate.
const ghCandidateListLimit = 1000

// ListCandidates returns open issues matching include/exclude labels
// and not carrying ClaimedLabel.
func (a *GitHubAdapter) ListCandidates(ctx context.Context) ([]Issue, error) {
	search := buildSearch(a.opts)
	args := []string{
		"issue", "list",
		"--repo", a.opts.Repo,
		"--state", "open",
		"--limit", fmt.Sprintf("%d", ghCandidateListLimit),
		"--json", "number,title,body,labels,state,assignees,author,createdAt,updatedAt,url",
	}
	if search != "" {
		args = append(args, "--search", search)
	}
	out, err := a.opts.Command(ctx, args, a.env())
	if err != nil {
		return nil, fmt.Errorf("gh issue list: %w", err)
	}
	var raw []ghIssue
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("gh issue list parse: %w", err)
	}
	if len(raw) >= ghCandidateListLimit && a.opts.Logger != nil {
		a.opts.Logger.Warn("github tracker: ListCandidates hit the %d-issue cap on repo %s — beyond this point issues are silently dropped from dispatch; consider tightening label filters",
			ghCandidateListLimit, a.opts.Repo)
	}
	if a.boardMode() {
		// The issue list stays the source of CONTENT; the board decides the
		// state and the eligibility.
		return a.listCandidatesFromBoard(ctx, raw)
	}
	// Open-issue set for fail-open blocker resolution (all open issues this
	// list returned, before eligibility filtering).
	openNums := make(map[int]bool, len(raw))
	for _, r := range raw {
		openNums[r.Number] = true
	}
	pending := make([]Issue, 0, len(raw))
	for _, r := range raw {
		if ghHasLabel(r.Labels, a.opts.ClaimedLabel) {
			continue // already claimed by a dispatcher — not a candidate
		}
		if !a.authorAllowed(r.Author.Login) {
			continue // author not in the trusted-author allowlist
		}
		iss := a.toIssue(r)
		if iss.WorkflowState == "" {
			continue // doesn't match any configured state
		}
		pending = append(pending, iss)
	}
	// Hold any issue whose body declares a still-open blocker (fail-open).
	return filterHeldByBlockers(pending, openNums, a.opts.Logger, "github"), nil
}

// authorAllowed reports whether an issue author passes AuthorAllowlist
// (case-insensitive). An empty allowlist admits any author.
func (a *GitHubAdapter) authorAllowed(login string) bool {
	if len(a.opts.AuthorAllowlist) == 0 {
		return true
	}
	for _, allowed := range a.opts.AuthorAllowlist {
		if strings.EqualFold(allowed, login) {
			return true
		}
	}
	return false
}

// RefreshStates returns the current state for each ID (which on the
// GitHub side means: read the current labels and re-derive the
// state_mapping result).
//
// One `gh api` call covers the entire set instead of spawning one `gh
// issue view <num>` per ID. The trade-off: GH returns 100 issues max
// per page, which is enough for any realistic dispatcher's running set
// (gated by agent.max_concurrent, typically single digits).
func (a *GitHubAdapter) RefreshStates(ctx context.Context, ids []string) (map[string]string, error) {
	if len(ids) == 0 {
		return map[string]string{}, nil
	}
	if a.boardMode() {
		// One board read answers the whole set — no per-issue REST call.
		return a.refreshStatesFromBoard(ctx, ids)
	}
	wanted := make(map[int]string, len(ids))
	for _, id := range ids {
		if num, ok := parseGitHubID(a.opts.Repo, id); ok {
			wanted[num] = id
		}
	}
	if len(wanted) == 0 {
		return map[string]string{}, nil
	}

	// Fetch each issue we care about individually rather than scanning
	// the whole repo. The previous single-page repo scan silently
	// truncated at 100 issues — repos with more open issues than
	// per_page caused running-but-not-on-page-1 issues to appear as
	// "disappeared", which the dispatcher would then cancel. Fetching
	// by ID is O(running set), which is bounded by MaxConcurrent.
	out := make(map[string]string, len(wanted))
	for num, id := range wanted {
		args := []string{
			"api",
			fmt.Sprintf("repos/%s/issues/%d", a.opts.Repo, num),
			"-H", "Accept: application/vnd.github+json",
		}
		raw, err := a.opts.Command(ctx, args, a.env())
		if err != nil {
			// Don't fail the whole sweep on a single issue's transient
			// error — log + skip so the other running issues keep
			// their state. Logging instead of swallowing silently was
			// the agent's specific complaint.
			if a.opts.Logger != nil {
				a.opts.Logger.Warn("dispatcher: github RefreshStates: gh api issue %d: %v", num, err)
			}
			continue
		}
		var r apiIssue
		if err := json.Unmarshal(raw, &r); err != nil {
			if a.opts.Logger != nil {
				a.opts.Logger.Warn("dispatcher: github RefreshStates: parse issue %d: %v", num, err)
			}
			continue
		}
		iss := a.toIssue(r.toGhIssue())
		if iss.WorkflowState != "" {
			out[id] = iss.WorkflowState
		}
	}
	return out, nil
}

// apiIssue mirrors the REST shape that `gh api repos/.../issues`
// returns, which differs slightly from `gh issue list --json` (camelCase
// vs snake_case fields). The two are converged into ghIssue here so the
// rest of the adapter stays uniform.
type apiIssue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	Labels    []ghLabel `json:"labels"`
	Assignees []ghUser  `json:"assignees"`
	User      ghUser    `json:"user"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	HTMLURL   string    `json:"html_url"`
}

func (a apiIssue) toGhIssue() ghIssue {
	return ghIssue{
		Number:    a.Number,
		Title:     a.Title,
		Body:      a.Body,
		State:     a.State,
		Labels:    a.Labels,
		Assignees: a.Assignees,
		Author:    a.User,
		CreatedAt: a.CreatedAt,
		UpdatedAt: a.UpdatedAt,
		URL:       a.HTMLURL,
	}
}

// UpdateState transitions an issue by adjusting labels per the
// matching state mapping — or, in board mode, by writing the project's
// Status field. Best-effort: if newState maps to neither, returns
// ErrTransitionRejected.
func (a *GitHubAdapter) UpdateState(ctx context.Context, id, newState string) error {
	if a.boardMode() {
		return a.updateStateOnBoard(ctx, id, newState)
	}
	sel, err := resolveLabelSelector(a.opts.StateMapping, newState)
	if err != nil {
		return err
	}
	num, ok := parseGitHubID(a.opts.Repo, id)
	if !ok {
		return ErrNotFound
	}
	args := []string{"issue", "edit", fmt.Sprintf("%d", num), "--repo", a.opts.Repo}
	for _, l := range sel.LabelsInclude {
		args = append(args, "--add-label", l)
	}
	for _, l := range sel.LabelsExclude {
		args = append(args, "--remove-label", l)
	}
	if _, err := a.opts.Command(ctx, args, a.env()); err != nil {
		return fmt.Errorf("gh issue edit: %w", err)
	}
	return nil
}

// Comment appends a note on the issue.
func (a *GitHubAdapter) Comment(ctx context.Context, id, body string) error {
	num, ok := parseGitHubID(a.opts.Repo, id)
	if !ok {
		return ErrNotFound
	}
	args := []string{"issue", "comment", fmt.Sprintf("%d", num), "--repo", a.opts.Repo, "--body", body}
	if _, err := a.opts.Command(ctx, args, a.env()); err != nil {
		return fmt.Errorf("gh issue comment: %w", err)
	}
	return nil
}

// Claim adds the ClaimedLabel. The marker is NOT persisted anywhere on
// the issue (labels carry no host/pid — see the Tracker interface note),
// which is why the boot journal is this adapter's only claim-recovery
// path.
func (a *GitHubAdapter) Claim(ctx context.Context, id, marker string) error {
	num, ok := parseGitHubID(a.opts.Repo, id)
	if !ok {
		return ErrNotFound
	}
	args := []string{"issue", "edit", fmt.Sprintf("%d", num), "--repo", a.opts.Repo, "--add-label", a.opts.ClaimedLabel}
	if _, err := a.opts.Command(ctx, args, a.env()); err != nil {
		return fmt.Errorf("gh issue edit (claim): %w", err)
	}
	return nil
}

// Release removes the ClaimedLabel. Idempotent — gh ignores
// remove-label for a label that isn't present, and a MISSING issue
// (deleted, transferred) maps to ErrNotFound like the Forgejo twin:
// callers treat that absence as benign, and without the mapping a
// deleted issue's claim-journal entry was retried and warned at every
// boot, for ever.
func (a *GitHubAdapter) Release(ctx context.Context, id, marker string) error {
	num, ok := parseGitHubID(a.opts.Repo, id)
	if !ok {
		return ErrNotFound
	}
	args := []string{"issue", "edit", fmt.Sprintf("%d", num), "--repo", a.opts.Repo, "--remove-label", a.opts.ClaimedLabel}
	if _, err := a.opts.Command(ctx, args, a.env()); err != nil {
		if a.ghReleaseGone(err) {
			return ErrNotFound
		}
		return fmt.Errorf("gh issue edit (release): %w", err)
	}
	return nil
}

// ghReleaseGone recognises the gh CLI error texts that mean the release
// target is PERMANENTLY absent: the issue itself (GraphQL resolve
// failure, REST 404), or the claim LABEL deleted from the repo — the
// second member of the same class: either way the claim cannot exist
// any more, and a non-benign error would keep the journal entry retried
// and warned at every boot, for ever. The label form is anchored on the
// exact configured label name (gh 2.x prints `'<label>' not found`), so
// an unrelated not-found in the message cannot match. Text matching is
// brittle but the CLI offers no typed channel.
func (a *GitHubAdapter) ghReleaseGone(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "could not resolve to an issue") ||
		strings.Contains(msg, "not found (http 404)") ||
		strings.Contains(msg, strings.ToLower("'"+a.opts.ClaimedLabel+"' not found"))
}

// HasLinkedPR reports whether an OPEN pull request already references this
// issue with a "#N" link — the same signal GitHub's own auto-close and the
// board projection use (forge.ParseIssueRefs over each PR's title+body). The
// deterministic ticket router reads it to decide whether a fresh issue needs
// implementing (no PR) or is already owned by the inbound PR-webhook (a PR
// links it → the dispatcher steps aside). Satisfies the optional linkedPRProbe
// capability the dispatcher type-asserts. One `gh pr list` per call; the repo's
// PR count is small in practice.
func (a *GitHubAdapter) HasLinkedPR(ctx context.Context, id string) (bool, error) {
	num, ok := parseGitHubID(a.opts.Repo, id)
	if !ok {
		return false, ErrNotFound
	}
	args := []string{"pr", "list", "--repo", a.opts.Repo, "--state", "open", "--json", "number,title,body", "--limit", "200"}
	out, err := a.opts.Command(ctx, args, a.env())
	if err != nil {
		return false, fmt.Errorf("gh pr list (linked-pr probe): %w", err)
	}
	var prs []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
	}
	if err := json.Unmarshal(out, &prs); err != nil {
		return false, fmt.Errorf("gh pr list decode: %w", err)
	}
	for _, pr := range prs {
		for _, ref := range forge.ParseIssueRefs(true, pr.Title, pr.Body) {
			if ref == num {
				return true, nil
			}
		}
	}
	return false, nil
}

// ApplyLabel adds a label to the issue (the visible bot:featurly / bot:billy
// association the ticket router stamps). Reuses the same `gh issue edit
// --add-label` seam as Claim; idempotent (gh no-ops an already-present label).
// Satisfies the optional labelApplier capability.
func (a *GitHubAdapter) ApplyLabel(ctx context.Context, id, label string) error {
	num, ok := parseGitHubID(a.opts.Repo, id)
	if !ok {
		return ErrNotFound
	}
	args := []string{"issue", "edit", fmt.Sprintf("%d", num), "--repo", a.opts.Repo, "--add-label", label}
	if _, err := a.opts.Command(ctx, args, a.env()); err != nil {
		return fmt.Errorf("gh issue edit (apply-label %q): %w", label, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// internals
// ---------------------------------------------------------------------------

// ghIssue is the JSON subset we ask gh to emit.
type ghIssue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	Labels    []ghLabel `json:"labels"`
	Assignees []ghUser  `json:"assignees"`
	Author    ghUser    `json:"author"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	URL       string    `json:"url"`
}

type ghLabel struct {
	Name string `json:"name"`
}

// ghHasLabel reports whether the issue carries the named label.
func ghHasLabel(labels []ghLabel, want string) bool {
	if want == "" {
		return false
	}
	for _, l := range labels {
		if l.Name == want {
			return true
		}
	}
	return false
}

type ghUser struct {
	Login string `json:"login"`
}

func (a *GitHubAdapter) toIssue(g ghIssue) Issue {
	labels := make([]string, 0, len(g.Labels))
	for _, l := range g.Labels {
		labels = append(labels, l.Name)
	}
	// Exclude the claimed label from the surfaced labels so dispatch
	// templates render a stable view.
	labels = filterOutString(labels, a.opts.ClaimedLabel)

	id := fmt.Sprintf("github:%s#%d", a.opts.Repo, g.Number)
	assignee := ""
	if len(g.Assignees) > 0 {
		assignee = g.Assignees[0].Login
	}
	state := resolveStateByLabels(labels, a.opts.StateMapping)

	return Issue{
		ID:            id,
		Identifier:    fmt.Sprintf("%s#%d", a.opts.Repo, g.Number),
		Title:         g.Title,
		Body:          g.Body,
		WorkflowState: state,
		CreatedAt:     g.CreatedAt,
		UpdatedAt:     g.UpdatedAt,
		Labels:        labels,
		Assignee:      assignee,
		Metadata: map[string]string{
			"url":          g.URL,
			"github_state": g.State,
			"author":       g.Author.Login,
		},
	}
}

func (a *GitHubAdapter) env() []string {
	if a.opts.Token == "" {
		return nil
	}
	return []string{"GH_TOKEN=" + a.opts.Token, "GITHUB_TOKEN=" + a.opts.Token}
}

// buildSearch composes the --search query from include/exclude label
// hints. gh search supports `label:foo -label:bar` syntax.
func buildSearch(opts GitHubOptions) string {
	parts := make([]string, 0, len(opts.IncludeLabels)+len(opts.ExcludeLabels)+1)
	for _, l := range opts.IncludeLabels {
		parts = append(parts, "label:"+quoteLabel(l))
	}
	for _, l := range opts.ExcludeLabels {
		parts = append(parts, "-label:"+quoteLabel(l))
	}
	// The claimed label is filtered CLIENT-side (ListCandidates), not here.
	// Excluding it server-side would also drop claimed issues from the
	// open-issue set the dependency gate resolves blockers against — so an
	// issue "Blocked by #7" would dispatch the moment #7 was claimed, i.e.
	// exactly while it is being implemented. Matches gitlab.go / forgejo.go.
	return strings.Join(parts, " ")
}

func quoteLabel(l string) string {
	if strings.ContainsAny(l, " \t") {
		return `"` + l + `"`
	}
	return l
}

// parseGitHubID rejects non-positive issue numbers: a "#-5" would become a
// `gh issue edit -5 …` flag injection.
func parseGitHubID(repo, id string) (int, bool) {
	return parsePrefixedID("github:"+repo+"#", id)
}

// runGH is the default Command — shells out to the user's `gh` install.
// stderr is bubbled up as part of the error so users see "gh: bad
// credentials" rather than an opaque exit code.
func runGH(ctx context.Context, args []string, env []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	if env != nil {
		// Restrict the inherited environment to the variables gh and
		// its children (git, ssh, openssl, …) actually need. Pulling
		// in the full parent env via os.Environ() would expose every
		// secret iterion holds (ANTHROPIC_API_KEY, OPENAI_API_KEY,
		// FORGEJO_TOKEN, …) to gh's subprocesses via /proc/PID/environ,
		// readable by any same-uid process. Note: GH_TOKEN itself
		// remains inheritable to gh's direct subprocesses — that is
		// unavoidable without writing to gh's on-disk credentials
		// file (out of scope here, see docs/dispatcher.md).
		cmd.Env = append(inheritedGHEnv(), env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("%s", redactGHSecrets(msg))
	}
	return stdout.Bytes(), nil
}

// ghEnvAllowlist names the host environment variables propagated to
// `gh` invocations. Anything not listed here is dropped so unrelated
// secrets in iterion's env don't leak to gh's subprocesses.
var ghEnvAllowlist = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TMP", "TEMP",
	"LANG", "LC_ALL", "LC_CTYPE", "LC_MESSAGES", "TZ", "TERM",
	"GH_CONFIG_DIR", "GH_HOST", "GH_REPO", "GH_EDITOR", "GH_BROWSER",
	"GH_PAGER", "GH_PROMPT_DISABLED", "GH_NO_UPDATE_NOTIFIER",
	"GH_DEBUG", "GH_FORCE_TTY", "GH_MDWIDTH",
	"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY",
	"https_proxy", "http_proxy", "no_proxy",
	"SSH_AUTH_SOCK", "SSH_AGENT_PID",
	"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR",
	"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_AUTHOR_NAME",
	"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
	"GIT_SSH", "GIT_SSH_COMMAND", "GIT_TERMINAL_PROMPT",
}

func inheritedGHEnv() []string {
	out := make([]string, 0, len(ghEnvAllowlist))
	for _, k := range ghEnvAllowlist {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// redactGHSecrets blanks out token-shaped substrings the gh CLI may
// echo back on failure (e.g. "Invalid token: ghp_xxxx…"). Without
// this, a misconfigured GH_TOKEN leaks via the bubbled-up error into
// downstream logs and centralized log aggregation.
func redactGHSecrets(s string) string {
	for _, prefix := range ghTokenPrefixes {
		for {
			i := strings.Index(s, prefix)
			if i < 0 {
				break
			}
			// Trim everything from the prefix to the next whitespace
			// or end-of-string and replace with a redaction marker.
			tail := s[i+len(prefix):]
			end := strings.IndexAny(tail, " \t\n\r\"'")
			if end < 0 {
				end = len(tail)
			}
			s = s[:i] + prefix + "***REDACTED***" + tail[end:]
		}
	}
	return s
}

// ghTokenPrefixes lists the documented prefixes GitHub uses for
// personal access tokens, OAuth tokens, server-to-server tokens, and
// fine-grained tokens. Keep in sync with
// https://github.blog/2021-04-05-behind-githubs-new-authentication-token-formats/
var ghTokenPrefixes = []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"}
