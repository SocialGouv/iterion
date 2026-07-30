package forge

import "context"

// statusDescriptionMax is GitHub's hard cap on a commit-status description
// (140 chars); GitLab/Forgejo are more lenient, so this bound is safe for all.
const statusDescriptionMax = 140

// TruncateStatusDescription clamps a status description to the forge-safe
// length, appending an ellipsis when it overflows.
func TruncateStatusDescription(s string) string {
	r := []rune(s)
	if len(r) <= statusDescriptionMax {
		return s
	}
	return string(r[:statusDescriptionMax-1]) + "…"
}

// ---------------------------------------------------------------------------
// Commit-status publishing — the deterministic seam behind a bot-driven merge
// gate. A reviewer bot (e.g. Revi) posts a named status onto the PR head SHA;
// branch protection lists that status context as a required check, so the
// merge queue blocks until it is green. The status is a SEPARATE signal from
// the review comments (reviews stay non-blocking advice, see NewReview) — the
// gate lives entirely in this status, computed deterministically from the
// finding set, never from the LLM review verdict.
// ---------------------------------------------------------------------------

// CommitState is the normalized commit-status state posted onto a PR head,
// mapped per-provider to that forge's own vocabulary.
type CommitState string

const (
	// CommitStatePending — a status was created but the check is still
	// in-flight (reserved; reviewer bots post a terminal state).
	CommitStatePending CommitState = "pending"
	// CommitStateSuccess — the gate passes (no blocking findings).
	CommitStateSuccess CommitState = "success"
	// CommitStateFailure — the gate fails (≥1 blocking finding); merge is
	// blocked until the author addresses them (a re-review flips it) or a
	// maintainer overrides.
	CommitStateFailure CommitState = "failure"
	// CommitStateError — the check could not be evaluated (reserved).
	CommitStateError CommitState = "error"
)

// CommitStatus is a status to post on a commit SHA. Context is the check name
// branch protection matches on (e.g. "revi/review"); Description is the short
// human-readable line the forge renders next to the check; TargetURL links to
// the evidence (the posted review).
type CommitStatus struct {
	State       CommitState
	Context     string
	Description string
	TargetURL   string
}

// CommitStatusClient is the optional capability to POST a commit status on a
// PR head SHA — implemented by the github, gitlab and forgejo admin clients.
// The three forges expose the same primitive (GitHub commit-status API /
// GitLab commit status / Forgejo commit status), so the gate is forge-agnostic.
type CommitStatusClient interface {
	SetCommitStatus(ctx context.Context, repo, sha string, st CommitStatus) error
}

// CommitStatusLister reads the statuses already on a commit. Optional: a
// provider without it can still gate, it just cannot be repaired after the
// fact — a reconciler that cannot read must not write, or it would overwrite
// a real verdict with a synthetic one.
type CommitStatusLister interface {
	ListCommitStatuses(ctx context.Context, repo, sha string) ([]CommitStatus, error)
}
