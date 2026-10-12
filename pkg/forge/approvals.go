package forge

import (
	"context"
	"errors"
)

// ---------------------------------------------------------------------------
// Merge-gate gestures — cast an approval, arm auto-merge, request reviewers.
// The deterministic counterpart of a review: a bot's verdict is executed
// through the server publish surface, which holds the grant (manifest
// scopes), the stale-SHA guard and the verdict ordering; this layer only
// speaks the gesture. Reviews stay comment-only (see NewReview); gating is
// this separately-declared capability set, implemented where the forge has a
// native gesture. A provider that lacks one simply does not implement the
// interface — same optional-capability contract as ReviewClient.
// ---------------------------------------------------------------------------

// ApprovalResult reports a cast approval. Verified is true only when the
// provider confirmed an approval exists on the MR (a successful create, or a
// duplicate-approve refusal recognized as a confirmed no-op); a refusal whose
// meaning could not be pinned returns an error, never Verified=false with
// nil error — a caller must not have to guess.
type ApprovalResult struct {
	// URL links the MR (the approval itself has no page on either provider).
	URL string
}

// MergeApprover casts the client account's approval on an open pull/merge
// request. sha, when non-empty, pins the approval to that head revision
// where the provider supports it (GitLab's approve endpoint takes it; GitHub
// reviews are always anchored to the current head) — the forge-side twin of
// the audited-SHA guard the publish surface already applies to the gate
// status. Must be idempotent: approving twice is a confirmed no-op, not an
// error the caller would read as "not approved". An approval refused for
// want of eligibility (project approval rules, role) surfaces as
// *PermissionError, so the operator reads the rule to relax, not a 403.
type MergeApprover interface {
	ApprovePullRequest(ctx context.Context, repo string, number int, sha string) (ApprovalResult, error)
}

// AutoMergeOptions pins an auto-merge arming to a head revision.
type AutoMergeOptions struct {
	// Method is how the forge integrates the change when the pipeline goes
	// green. Squash and merge are the only methods every provider arms;
	// rebase is not an auto-merge method on any of them.
	Method MergeMethod
	// SHA is REQUIRED: the audited head the arming was decided on. The
	// forge refuses (ErrStaleHead) when the PR's head has moved — an
	// unpinned arming is the unaudited-commit-reaches-main shape, and no
	// implementation accepts one.
	SHA string
	// DeleteBranch removes the source branch after the merge completes.
	DeleteBranch bool
}

// AutoMergeResult reports what the arming actually produced.
type AutoMergeResult struct {
	// State is "mwps" (armed: the forge merges when the pipeline succeeds)
	// or "merged" (the pipeline was already green — the forge merged on the
	// spot). Nothing else: a caller that cannot distinguish the two reads
	// one as the other's superset, which is exactly the confusion this
	// field exists to prevent.
	State string
	// URL links the MR; SHA repeats the head the forge will merge when
	// ready.
	URL string
	SHA string
}

// AutoMerge armings are decided against these two states.
const (
	AutoMergeArmed  = "mwps"
	AutoMergeMerged = "merged"
)

// AutoMergeArmer arms merge-when-pipeline-succeeds (GitLab) / auto-merge
// (GitHub): the forge itself performs the merge once its own merge
// conditions hold. Arming is NOT merging — there is deliberately no
// merge-now gesture in this capability set; a bot that wants the merge to
// happen arms and lets the forge gate it.
type AutoMergeArmer interface {
	ArmAutoMerge(ctx context.Context, repo string, number int, opts AutoMergeOptions) (AutoMergeResult, error)
}

// PullReviewerSetter adds named accounts to a pull/merge request's reviewer
// set — the escalation gesture of a gating bot ("needs a human review").
// Additive-safe BY CONTRACT, mirroring ReviewerAssigner and
// WithdrawPullReviewRequests: a read-modify-write union that can never drop
// a reviewer it was not named. Returns the logins actually added (an account
// already on the set is not "added"); a login the provider cannot resolve is
// an error (fail closed), never a silent skip.
type PullReviewerSetter interface {
	// AddPullReviewers resolves each login to a provider account and adds
	// the union to the reviewer set. Duplicate inputs are collapsed. The set
	// is the REQUESTED-reviewer set: on GitHub an account that already
	// reviewed (and is no longer pending) is re-requested — a duplicate ping,
	// never silently skipped; the caller names reviewers it wants eyes from.
	AddPullReviewers(ctx context.Context, repo string, number int, logins []string) ([]string, error)
}

// Mergeability is the forge's own merge verdict — what a gating bot reads
// before (or after) arming, and what the publish surface reports when a
// gesture is refused because the MR cannot take it. Every field degrades to
// zero-values/unknown rather than failing the read: the read is diagnostic,
// never load-bearing for arming (MWPS *is* the wait-for-it primitive).
type Mergeability struct {
	// MergeStatus is the normalized verdict: "mergeable", "blocked",
	// "pending" or "unknown".
	MergeStatus string
	// Detailed is the provider-native status (GitLab detailed_merge_status),
	// kept verbatim for the operator.
	Detailed string
	// UnresolvedDiscussions counts threads a resolve-before-merge setting
	// would block on; unknown on providers that do not report it.
	UnresolvedDiscussions int
	// ApprovalsRequired / ApprovalsLeft are the project's approval rules as
	// the forge reports them; -1 = the provider does not say.
	ApprovalsRequired int
	ApprovalsLeft     int
}

// Mergeability normalized verdicts.
const (
	MergeabilityMergeable = "mergeable"
	MergeabilityBlocked   = "blocked"
	MergeabilityPending   = "pending"
	MergeabilityUnknown   = "unknown"
)

// MergeabilityReader is the optional read of that verdict.
type MergeabilityReader interface {
	GetMergeability(ctx context.Context, repo string, number int) (Mergeability, error)
}

// ErrStaleHead is a forge refusing a SHA-pinned mutation because the PR's
// head moved since the audited revision (GitLab merge 406 "SHA does not
// match HEAD of source branch"). The publish surface reports it as a
// refusal — the verdict is stale and a fresh one must decide — never a
// retry.
var ErrStaleHead = errors.New("forge: head moved since the audited revision")

// ErrNotMergeable is a forge refusing a merge/arming because the merge
// request is not in a state that can take one (conflicts, unresolved
// blocking threads, closed). Distinct from ErrStaleHead: the head may be
// exactly what was audited and the MR still unmergeable.
var ErrNotMergeable = errors.New("forge: merge request is not in a mergeable state")
