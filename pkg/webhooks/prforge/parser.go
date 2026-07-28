package prforge

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// Parsed is the normalized PR view the inbound handler consumes. It
// mirrors gitlab.Parsed field-for-field so the per-provider handlers
// (GitHub + Forgejo/Gitea, both routed through this package) all look
// the same — SenderLogin is audit-only in V1 and not stored on the
// delivery row.
type Parsed struct {
	RepoID       int64
	ProjectPath  string // "owner/repo"
	CloneURL     string
	PRNumber     int64
	Action       string // "opened" | "reopened" | "synchronize" | "synchronized" | …
	SourceBranch string // head.ref
	TargetBranch string // base.ref
	Title        string
	Description  string
	PRURL        string
	HeadSHA      string
	State        string
	SenderLogin  string
	// AuthorLogin is who OPENED the pull request. On `opened` it equals
	// SenderLogin; on a push to an existing PR the sender is whoever pushed,
	// which is not whose PR it is. Author-based routing (which bot owns this
	// PR) must read THIS — otherwise a human pushing a fix to a dependency
	// bot's PR hands the delivery to the wrong bot. Empty when the payload
	// omits it; callers fall back to SenderLogin.
	AuthorLogin string
	// HeadRepoFullName is the "owner/repo" the PR's head branch lives in. It
	// differs from ProjectPath (the base repo) for a fork PR; empty when the
	// payload omits head.repo. Read by IsCrossRepo for the fork guard.
	HeadRepoFullName string
	// DequeueReason is the merge-queue eject reason on a `dequeued`
	// action (e.g. "MERGE_CONFLICT", "CI_FAILURE"). Empty otherwise.
	DequeueReason string
	// Draft reports whether the PR is a work-in-progress draft. A draft PR
	// never auto-triggers a bot (IsReviewable is false); the trigger is the
	// `ready_for_review` action that clears it.
	Draft bool
}

// healableDequeueReasons are the merge-queue eject reasons that a
// branch-improvement auto-heal can actually fix: a textual conflict with
// the queue head, or a combined-build/CI failure. Reasons like a manual
// dequeue or a queue reset are NOT healable (nothing to fix on the branch)
// — re-dispatching a bot on those would be waste or a loop.
var healableDequeueReasons = map[string]bool{
	"MERGE_CONFLICT":       true,
	"CI_FAILURE":           true,
	"INVALID_MERGE_COMMIT": true,
	"MERGE_CONFLICT_ERROR": true,
}

// NeedsAutoHeal reports whether this delivery is a merge-queue ejection an
// auto-heal bot should try to fix — the PR left the queue because its diff
// conflicts with, or breaks the combined build against, the current queue
// head. The webhook handler still applies the fork/allowlist/bot guards.
func (p Parsed) NeedsAutoHeal() bool {
	return p.Action == "dequeued" && healableDequeueReasons[p.DequeueReason]
}

// ParsePullRequest decodes a pull_request webhook body from GitHub or
// Forgejo/Gitea (one shared wire shape). We reject empty bodies / wrong
// shapes early so the handler can return a clean 400 instead of crashing
// on a nil deref. Defensive: tolerate missing top-level Number (some
// events nest it only inside pull_request).
func ParsePullRequest(body []byte) (Parsed, error) {
	var e PullRequestEvent
	if err := json.Unmarshal(body, &e); err != nil {
		return Parsed{}, fmt.Errorf("prforge: decode pull_request event: %w", err)
	}
	pr := e.PullRequest
	if pr.Number == 0 && e.Number != 0 {
		pr.Number = e.Number
	}
	return Parsed{
		RepoID:           e.Repository.ID,
		ProjectPath:      e.Repository.FullName,
		CloneURL:         e.Repository.CloneURL,
		PRNumber:         pr.Number,
		Action:           e.Action,
		SourceBranch:     pr.Head.Ref,
		TargetBranch:     pr.Base.Ref,
		Title:            pr.Title,
		Description:      pr.Body,
		PRURL:            pr.HTMLURL,
		HeadSHA:          pr.Head.SHA,
		State:            pr.State,
		SenderLogin:      e.Sender.Login,
		AuthorLogin:      pr.User.Login,
		HeadRepoFullName: pr.Head.Repo.FullName,
		DequeueReason:    e.Reason,
		Draft:            pr.Draft,
	}, nil
}

// Author returns the login author-based routing must use: the PR's own
// author, falling back to the event sender when a payload omits it (older
// Forgejo/Gitea versions, hand-crafted redeliveries).
func (p Parsed) Author() string {
	if p.AuthorLogin != "" {
		return p.AuthorLogin
	}
	return p.SenderLogin
}

// IsCrossRepo reports whether the PR's head branch lives in a DIFFERENT repo
// than its base — i.e. the PR comes from a fork. This is the fork-guard
// signal: a fork PR is untrusted, so the inbound handler must not auto-launch a
// MUTATING bot (which would run costly LLM work + push commits) on it without
// operator validation — the anti budget-exhaustion boundary. An empty head
// repo (minimal/legacy payloads) is treated as same-repo to avoid falsely
// gating a trusted internal PR.
func (p Parsed) IsCrossRepo() bool {
	return p.HeadRepoFullName != "" && p.HeadRepoFullName != p.ProjectPath
}

// IsReviewable reports whether the PR action should AUTO-trigger a
// review. A DRAFT PR is never auto-reviewable — the author is still
// iterating, and auto-running a bot on it wastes budget and churns an
// unfinished branch; the trigger is instead `ready_for_review` (which
// clears the draft flag). Otherwise: only opened / reopened. Subsequent
// push actions ("synchronize" on GitHub-shaped payloads, "synchronized"
// on Gitea-shaped payloads) deliberately do NOT re-trigger; re-review is
// on-demand.
func (p Parsed) IsReviewable() bool {
	if p.Draft {
		return false
	}
	switch p.Action {
	case "opened", "reopened", "ready_for_review":
		return true
	default:
		return false
	}
}

// IsSynchronize reports whether this is a push to the PR head — GitHub spells
// it "synchronize", Gitea/Forgejo "synchronized". A DRAFT PR is excluded (the
// author is still iterating; re-reviewing a WIP push wastes budget) — matching
// IsReviewable's draft guard and the GitLab IsSynchronize counterpart.
// IsReviewable deliberately excludes synchronize entirely (re-review is
// on-demand); the merge gate opts back in via the webhook's ReviewOnSync so
// the required status re-evaluates on each new head.
func (p Parsed) IsSynchronize() bool {
	return !p.Draft && (p.Action == "synchronize" || p.Action == "synchronized")
}

// SubjectID is the stable per-PR identifier used in delivery records.
func (p Parsed) SubjectID() string {
	return "pr:" + strconv.FormatInt(p.PRNumber, 10)
}
