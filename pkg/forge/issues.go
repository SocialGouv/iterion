package forge

import (
	"context"
	"strings"
	"time"
)

// This file defines the OPTIONAL issue / pull-request / CI capabilities a
// provider's admin client may expose, on top of the mandatory Admin interface
// (webhooks + repos). They are separate, type-asserted interfaces — exactly
// like OAuthAppProvisioner — so a provider that doesn't implement one yet
// degrades gracefully (the server checks `if ic, ok := admin.(IssueClient)`)
// instead of forcing every client to stub them.
//
// Powering:
//   - IssueClient → forge→board one-way sync (ListIssues) + board→forge
//     push-to-forge (CreateIssue / UpdateIssue).
//   - PullClient  → linked PRs + CI status (current + history) on board cards.
//
// `repo` is always the provider-native full name the rest of pkg/forge uses
// (github/forgejo: "owner/repo"; gitlab: "group/project" path, the client
// URL-encodes it). Numbers are the forge's own issue/MR iid.

// IssueRef is a normalized forge issue (or, on GitHub/Forgejo where the issues
// endpoint conflates them, possibly a pull request — see IsPullRequest).
type IssueRef struct {
	Number        int       `json:"number"`
	Title         string    `json:"title"`
	Body          string    `json:"body,omitempty"`
	State         string    `json:"state"` // "open" | "closed"
	URL           string    `json:"url"`
	Labels        []string  `json:"labels,omitempty"`
	Assignees     []string  `json:"assignees,omitempty"`
	Author        string    `json:"author,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	IsPullRequest bool      `json:"is_pull_request,omitempty"`
}

// IssueListOptions filters a ListIssues call. Zero value lists open issues,
// first page. Since enables incremental sync (only issues updated since the
// last sweep).
type IssueListOptions struct {
	State   string // "open" | "closed" | "all" (default "open")
	Labels  []string
	Page    int       // 1-based; 0 → 1
	PerPage int       // 0 → provider default (≈50)
	Since   time.Time // zero = no lower bound
}

// NewIssue is the payload for CreateIssue (board→forge push).
type NewIssue struct {
	Title     string
	Body      string
	Labels    []string
	Assignees []string
}

// IssuePatch is a partial update for UpdateIssue. Nil fields are left
// untouched; State is "open"|"closed".
type IssuePatch struct {
	Title     *string
	Body      *string
	State     *string
	Labels    *[]string
	Assignees *[]string
}

// CommentRef is a normalized issue/PR comment as the forge reports it after
// creation (board→forge / bot reply on the source issue/PR).
type CommentRef struct {
	ID        string    `json:"id"`
	URL       string    `json:"url,omitempty"`
	Body      string    `json:"body"`
	Author    string    `json:"author,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// IssueClient is the optional issue read/write capability. ListIssues powers
// forge→board sync; Create/UpdateIssue power per-card push-to-forge;
// CommentIssue lets a bot reply on the source issue/PR.
type IssueClient interface {
	ListIssues(ctx context.Context, repo string, opts IssueListOptions) ([]IssueRef, error)
	GetIssue(ctx context.Context, repo string, number int) (IssueRef, error)
	CreateIssue(ctx context.Context, repo string, in NewIssue) (IssueRef, error)
	UpdateIssue(ctx context.Context, repo string, number int, patch IssuePatch) (IssueRef, error)
	// CommentIssue posts a comment on an issue. On GitHub/Forgejo the issues
	// endpoint is shared with PRs, so the same call comments on a PR by its
	// number; on GitLab it targets an issue note. Returns the created comment.
	CommentIssue(ctx context.Context, repo string, number int, body string) (CommentRef, error)
}

// PullRef is a normalized pull/merge request.
type PullRef struct {
	Number       int       `json:"number"`
	Title        string    `json:"title"`
	State        string    `json:"state"` // "open" | "closed" | "merged"
	URL          string    `json:"url"`
	SourceBranch string    `json:"source_branch,omitempty"`
	TargetBranch string    `json:"target_branch,omitempty"`
	HeadSHA      string    `json:"head_sha,omitempty"`
	Author       string    `json:"author,omitempty"`
	Draft        bool      `json:"draft,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	// HeadRepoFullName is the "owner/repo" the PR's head branch lives in. It
	// differs from the base repo (the endpoint's own path parameter) for a
	// fork PR; empty when the provider omits it. Read by IsCrossRepo for the
	// fork guard on lanes that resolve a PR via the forge API and therefore
	// cannot rely on the webhook payload's own head.repo field.
	HeadRepoFullName string `json:"head_repo_full_name,omitempty"`
	// LinkedIssues are issue numbers this PR references / closes, best-effort
	// parsed from the title/body ("fixes #12", "Closes #7", "!?").
	LinkedIssues []int `json:"linked_issues,omitempty"`
}

// IsCrossRepo reports whether the PR's head branch lives in a DIFFERENT repo
// than baseRepo — the fork-guard signal, matching pkg/webhooks/prforge.Parsed's
// same-named method. Returns false when HeadRepoFullName is empty (a legacy
// payload / provider that omits it): a caller that must fail-closed on
// unknown fork status has to combine this with an emptiness check on
// HeadRepoFullName, or use SameRepoAs — which returns false on empty head.
// Passing an empty baseRepo returns false — this method judges cross-repo
// only when both sides are known.
func (p PullRef) IsCrossRepo(baseRepo string) bool {
	return !SameRepo(p.HeadRepoFullName, baseRepo) && p.HeadRepoFullName != "" && baseRepo != ""
}

// SameRepoAs reports whether the PR's head branch lives in the SAME repo as
// baseRepo — the fail-CLOSED counterpart to IsCrossRepo. Every launch pair
// combining `<base repo>.CloneURL + pr.SourceBranch` MUST clear this before
// dispatching: an empty head repo means the provider omitted the field OR
// the head repo was deleted/blocked, and launching on it aims the bot at
// repoURL=<base> repoRef=<head branch> — a fixer would push LLM commits to
// the BASE repo's branch of that name.
//
// Empty head repo → false (never proven safe). Empty base → false. Both set
// and case-insensitively equal → true. The command lane, the autofix lane
// and the gate-relaunch lane all consult this before launching.
func (p PullRef) SameRepoAs(baseRepo string) bool {
	return SameRepo(p.HeadRepoFullName, baseRepo)
}

// SameRepo reports whether two "owner/repo" identifiers name the same
// repository, case-insensitively (owner/repo names are uniquely
// case-insensitive on every supported forge). Empty on either side → false:
// "unknown" is never proven equal, so a caller that fails-closed inherits
// the safe answer for free.
//
// The one vocabulary behind every cross-repo predicate — PullRef.SameRepoAs
// / IsCrossRepo here, prforge.Parsed / prforge.ParsedReviewComment
// IsCrossRepo in pkg/webhooks/prforge — so "Owner/Repo" and "owner/repo"
// never disagree between the payload side and the API side.
func SameRepo(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return strings.EqualFold(a, b)
}

// PullListOptions filters ListPullRequests.
type PullListOptions struct {
	State   string // "open" | "closed" | "all" (default "open")
	Page    int
	PerPage int
	Since   time.Time
}

// CIRun is one CI execution (a GitHub check-run / GitLab pipeline-or-job /
// Forgejo Actions run / commit-status) normalized to a common shape.
type CIRun struct {
	Name       string    `json:"name"`
	Status     string    `json:"status"` // "queued" | "running" | "success" | "failed" | "cancelled" | "skipped"
	Conclusion string    `json:"conclusion,omitempty"`
	URL        string    `json:"url,omitempty"`
	SHA        string    `json:"sha,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

// CIStatus is the aggregate CI state for a commit/ref plus its current runs.
type CIStatus struct {
	SHA   string  `json:"sha"`
	State string  `json:"state"` // aggregate: "success" | "failed" | "running" | "pending" | "unknown"
	Runs  []CIRun `json:"runs,omitempty"`
}

// NewPull is the payload for CreatePull (open a PR/MR, bot-driven). Branches
// are provider-native names: SourceBranch is the head, TargetBranch the base.
type NewPull struct {
	Title        string
	Body         string
	SourceBranch string
	TargetBranch string
	Draft        bool
}

// PullPatch is a partial update for UpdatePull. Nil fields are left untouched;
// State is "open"|"closed".
type PullPatch struct {
	Title        *string
	Body         *string
	TargetBranch *string
	State        *string
}

// MergeMethod selects how MergePull integrates the PR/MR. Empty → provider
// default ("merge").
type MergeMethod string

const (
	MergeMerge  MergeMethod = "merge"
	MergeSquash MergeMethod = "squash"
	MergeRebase MergeMethod = "rebase"
)

// MergeMethodWire maps a MergeMethod onto the provider wire value
// ("merge"|"squash"|"rebase"); empty → "merge". GitHub (`merge_method`) and
// Gitea/Forgejo (the `Do` field) share this exact vocabulary, so both reuse
// this; GitLab expresses squash as a boolean instead and does not use it.
func MergeMethodWire(m MergeMethod) string {
	switch m {
	case MergeSquash:
		return "squash"
	case MergeRebase:
		return "rebase"
	default:
		return "merge"
	}
}

// MergeOptions controls MergePull. Zero value = provider-default merge,
// keeping the source branch.
type MergeOptions struct {
	Method        MergeMethod
	CommitTitle   string
	CommitMessage string
	// DeleteBranch removes the source branch after a successful merge.
	DeleteBranch bool
	// SHA, when set, guards the merge: the forge merges only if the PR head
	// still matches (race protection). Best-effort — providers without the
	// guard ignore it.
	SHA string
}

// PullClient is the optional PR + CI capability: surfacing linked PRs and CI
// state (current + history) on board cards, plus bot-driven PR lifecycle
// (open / update / merge).
type PullClient interface {
	// ListPullRequests lists PRs/MRs for a repo (for linking to issues + the
	// card PR panel).
	ListPullRequests(ctx context.Context, repo string, opts PullListOptions) ([]PullRef, error)
	// GetPullRequest fetches one PR/MR by number.
	GetPullRequest(ctx context.Context, repo string, number int) (PullRef, error)
	// CreatePull opens a new pull/merge request (bot-driven; ties a run's work
	// back to the source card).
	CreatePull(ctx context.Context, repo string, in NewPull) (PullRef, error)
	// UpdatePull applies a partial update (retarget, retitle, close/reopen).
	UpdatePull(ctx context.Context, repo string, number int, patch PullPatch) (PullRef, error)
	// MergePull merges a PR/MR and returns the updated PullRef (State "merged"
	// on success).
	MergePull(ctx context.Context, repo string, number int, opts MergeOptions) (PullRef, error)
	// GetCIStatus returns the CURRENT aggregate CI state + runs for a ref
	// (commit SHA or branch).
	GetCIStatus(ctx context.Context, repo, ref string) (CIStatus, error)
	// ListCIHistory returns recent CI runs for a ref/branch, newest first,
	// up to limit (0 → provider default).
	ListCIHistory(ctx context.Context, repo, ref string, limit int) ([]CIRun, error)
}

// Normalized CI/issue state vocabularies, so providers map onto a stable set.
const (
	CIPending   = "pending"
	CIRunning   = "running"
	CISuccess   = "success"
	CIFailed    = "failed"
	CICancelled = "cancelled"
	CISkipped   = "skipped"
	CIUnknown   = "unknown"
)

// PRReviewComment is one comment in a PR's review threads (an inline diff
// comment or a reply inside one). InReplyTo == 0 marks a thread root; every
// reply carries the root's id. Consumed by the inbound-webhook
// reply-in-thread conversational gate.
type PRReviewComment struct {
	ID        int64
	InReplyTo int64
	Body      string
	Path      string
	CreatedAt string
	Author    string
}
