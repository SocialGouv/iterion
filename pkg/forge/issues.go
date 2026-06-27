package forge

import (
	"context"
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

// IssueClient is the optional issue read/write capability. ListIssues powers
// forge→board sync; Create/UpdateIssue power per-card push-to-forge.
type IssueClient interface {
	ListIssues(ctx context.Context, repo string, opts IssueListOptions) ([]IssueRef, error)
	GetIssue(ctx context.Context, repo string, number int) (IssueRef, error)
	CreateIssue(ctx context.Context, repo string, in NewIssue) (IssueRef, error)
	UpdateIssue(ctx context.Context, repo string, number int, patch IssuePatch) (IssueRef, error)
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
	// LinkedIssues are issue numbers this PR references / closes, best-effort
	// parsed from the title/body ("fixes #12", "Closes #7", "!?").
	LinkedIssues []int `json:"linked_issues,omitempty"`
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
	Status     string    `json:"status"`     // "queued" | "running" | "success" | "failed" | "cancelled" | "skipped"
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

// PullClient is the optional PR + CI capability for surfacing linked PRs and
// CI state (current + history) on board cards.
type PullClient interface {
	// ListPullRequests lists PRs/MRs for a repo (for linking to issues + the
	// card PR panel).
	ListPullRequests(ctx context.Context, repo string, opts PullListOptions) ([]PullRef, error)
	// GetPullRequest fetches one PR/MR by number.
	GetPullRequest(ctx context.Context, repo string, number int) (PullRef, error)
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
