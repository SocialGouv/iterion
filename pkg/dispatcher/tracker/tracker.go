// Package tracker defines the issue-tracker abstraction used by the
// dispatcher (`iterion dispatch`). The dispatcher polls a Tracker
// implementation for eligible issues and dispatches a workflow run per
// issue. Three implementations ship with iterion: native (default,
// owned by iterion), github, and forgejo.
package tracker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Issue is the normalized shape of a tracker issue passed across the
// dispatcher boundary. Adapter-specific fields go into Metadata; native
// custom fields go into Fields.
type Issue struct {
	// ID is the stable, globally-unique identifier the dispatcher uses
	// to key its state. Implementations namespace this:
	//   native:<uuid>
	//   github:<owner>/<repo>#<number>
	//   forgejo:<host>/<owner>/<repo>#<number>
	ID string

	// Identifier is a short human-readable label (e.g. "repo#123" or
	// the first 8 chars of a native UUID).
	Identifier string

	Title string
	Body  string

	// WorkflowState is the user-defined state name (e.g. "ready",
	// "in_progress", "done"). Each Tracker maps its native concept of
	// state to this string.
	WorkflowState string

	// Priority — higher value sorts earlier in the dispatch queue.
	// Trackers that have no priority concept return 0.
	Priority int

	CreatedAt time.Time
	UpdatedAt time.Time

	Labels   []string
	Assignee string

	// Blockers is the list of issue IDs that must reach a terminal
	// state before this issue becomes eligible. Empty when unknown or
	// unsupported by the tracker.
	Blockers []string

	// Fields holds typed custom-field values. Used by the native
	// tracker; external adapters leave it nil.
	Fields map[string]any

	// Bot, when non-empty, names the bot the operator picked on the
	// ticket itself — the dispatcher uses it to override the per-
	// assignee / global workflow selection. Only the native tracker
	// populates this today; github/forgejo leave it empty.
	Bot string

	// BotArgs are per-ticket workflow var overrides merged on top of
	// the dispatcher config's templated vars at launch time (key by
	// key: BotArgs wins for declared keys). String-valued — the
	// engine handles coercion via the workflow's declared types.
	BotArgs map[string]string

	// Metadata holds adapter-specific extras (e.g. github URL,
	// milestone, html_url). Keep keys lowercase snake_case.
	Metadata map[string]string
}

// Tracker is the contract every issue-source adapter must satisfy.
//
// All methods take a context; implementations must honor cancellation.
// Implementations should be safe for concurrent calls — the dispatcher
// will invoke ListCandidates and RefreshStates from its actor goroutine
// while UpdateState/Comment/Claim may be triggered from HTTP handlers
// or background goroutines.
type Tracker interface {
	// Name returns a stable adapter name ("native", "github",
	// "forgejo"). Used for log lines and the dashboard.
	Name() string

	// ListCandidates returns issues currently eligible for dispatch,
	// already filtered by the adapter's configured eligibility rules
	// (state, label allowlist/blocklist, assignee, …). Order is
	// indicative; the dispatcher re-sorts by Priority + CreatedAt.
	ListCandidates(ctx context.Context) ([]Issue, error)

	// RefreshStates returns the current WorkflowState for each
	// requested ID. IDs that no longer exist on the tracker are
	// omitted from the returned map — callers should treat absence as
	// "issue disappeared".
	RefreshStates(ctx context.Context, ids []string) (map[string]string, error)

	// UpdateState transitions an issue to the given new state. Returns
	// ErrTransitionRejected if the state is unknown or the transition
	// is invalid.
	UpdateState(ctx context.Context, id, newState string) error

	// Comment appends a free-form note to the issue. Used by hooks and
	// the dispatcher to leave a trail of dispatch/finish events. Adapters
	// that don't support comments return ErrNotSupported.
	Comment(ctx context.Context, id, body string) error

	// Claim marks an issue as taken by the given marker (typically
	// "<hostname>-<pid>"). Native trackers store the marker in the
	// issue record and enforce it (ErrClaimConflict on collision);
	// external adapters (GitHub/GitLab/Forgejo) just add the
	// configured ClaimedLabel — marker is accepted for interface
	// parity but not recorded or checked, so those forges get no
	// conflict detection.
	Claim(ctx context.Context, id, marker string) error

	// Release removes the claim marker. Idempotent — releasing an
	// unclaimed issue is not an error.
	Release(ctx context.Context, id, marker string) error
}

// ClaimToken is the proof of claim ownership a board store hands back
// from Claim and demands on every owner-scoped write. The Epoch is a
// per-issue fencing counter bumped on every fresh acquisition (never on
// an idempotent same-marker re-claim): a worker whose claim was stolen
// presents a stale epoch and every late write of its finds a typed
// refusal instead of clobbering the new owner's state — the ADR-094
// effect-outbox contract, applied to the board claim.
//
// It lives in this package (not native) because native imports tracker
// for the shared error vocabulary, and the dispatcher-facing capability
// below needs the type too.
type ClaimToken struct {
	Marker string
	Epoch  int64
}

// ClaimLeaser is the optional tracker capability for backends whose
// claims carry a persisted lease + fencing epoch (the native and Mongo
// boards). Forge adapters (GitHub/GitLab/Forgejo) do NOT implement it —
// their label-based claims have no record to fence — and the dispatcher
// must detect that once, say so in its log, and keep the legacy
// unfenced path for them rather than silently no-oping lease renewals.
type ClaimLeaser interface {
	// ClaimLease is Claim returning the ownership token.
	ClaimLease(ctx context.Context, id, marker string) (ClaimToken, error)
	// RenewClaim extends the claim's lease. ErrClaimConflict when the
	// token no longer owns the issue — the caller must stop writing.
	RenewClaim(ctx context.Context, id string, tok ClaimToken) error
	// ReleaseOwned releases under the token (fenced Release).
	ReleaseOwned(ctx context.Context, id string, tok ClaimToken) error
	// UpdateStateOwned is UpdateState fenced on the token.
	UpdateStateOwned(ctx context.Context, id, newState string, tok ClaimToken) error
}

// ExpiredClaim is one reap candidate: a card whose claim's lease ran out
// with nobody renewing. Prev is the claim AS LISTED — the reclaim CAS
// re-verifies it, so a claim that moved on between the list and the act
// is simply skipped.
type ExpiredClaim struct {
	IssueID    string
	Identifier string
	State      string
	LastRunID  string
	// LifetimeRuns is how many runs this card has EVER carried — the
	// append-only history, whatever launched them (operator re-queues,
	// dispatcher retries, fork adoptions). It is not a count of watchdog
	// reparks and must not be used as one: a card worked on three times is
	// perfectly healthy. Its only use is as a coarse spend backstop on the
	// surface where returning a card to the pool costs a fresh run.
	LifetimeRuns int
	// ClaimedAt is when the claim was acquired. It is what bounds the
	// watchdog's conservative rows: "wait, a run stamp may still be in
	// flight" is true for seconds, not forever.
	ClaimedAt time.Time
	Prev      ClaimToken
}

// ClaimReaper is the optional tracker capability behind the claim
// watchdog: list the cards whose lease expired, and TRANSFER one to a
// recovery owner. The transfer (never a bare clear) is what closes the
// double-launch window: an in_progress card freed before its disposition
// is decided is immediately re-dispatchable, and the reaper would be
// racing the tick it exists to clean up after.
type ClaimReaper interface {
	// ListExpiredClaimCandidates returns cards with a non-empty claim
	// whose lease expired before cutoff. LEGACY claims (no lease stamped)
	// are never listed — time cannot prove anything about them.
	ListExpiredClaimCandidates(ctx context.Context, cutoff time.Time, limit int) ([]ExpiredClaim, error)
	// ReclaimExpired CAS-transfers the claim from prev to marker iff the
	// claim is still exactly prev AND still expired at cutoff, bumping
	// the fencing epoch (the old owner's late writes die at the fence).
	// ErrClaimConflict when the claim moved on.
	//
	// It also returns the card's state AS OBSERVED BY THE CAS. That is
	// the only instant at which the state and the ownership are known
	// together: the state carried on ExpiredClaim was read at listing
	// time, and an operator can move the card between the listing and
	// the transfer — deciding the card's disposition on the stale value
	// overwrites exactly the intent the watchdog is supposed to honour.
	ReclaimExpired(ctx context.Context, id string, prev ClaimToken, marker string, cutoff time.Time) (ClaimToken, string, error)
}

// ReaperMarkerPrefix namespaces the claim marker a watchdog writes. It
// lives in this leaf package because provenance has to be readable where
// the write happens: a store holding the claim can then say that a state
// change came from a MACHINE repairing a dead owner, not from an
// operator or a bot — and downstream, a one-shot label gate can decline
// to be spent by it.
const ReaperMarkerPrefix = "reaper:"

// IsReaperMarker reports whether a claim marker was minted by a watchdog.
func IsReaperMarker(marker string) bool { return strings.HasPrefix(marker, ReaperMarkerPrefix) }

// ReasonWatchdog is the provenance stamped on a board event a watchdog
// caused. Consumers that act on operator intent (launching a bot,
// consuming a one-shot label) must not treat it as one.
const ReasonWatchdog = "watchdog"

// The other board-event reasons, named so the machine set below is an
// enumeration over constants rather than a scatter of literals.
const (
	// Schema migrations: the operator edits the BOARD, and the store
	// emits one event per touched card — none of those cards moved by an
	// operator gesture of its own.
	ReasonStateRename = "state_rename"
	ReasonStateDelete = "state_delete"
	ReasonFieldRename = "field_rename"
	ReasonFieldDelete = "field_delete"
	// ReasonUnblocked is the auto-promote CASCADE of closing a card's
	// last blocker — descriptive provenance, NOT machine: the close that
	// caused it is a deliberate gesture, and a subscription armed on the
	// promoted card's target state is exactly the intent it expresses.
	ReasonUnblocked = "unblocked"
	// ReasonRunFinished / ReasonRunFailed mark a watchdog TERMINAL filing
	// — the verdict the dead owner would have written. Descriptive, NOT
	// machine: the disposition is determined by the run's own outcome,
	// and a downstream subscription must fire exactly as it would have
	// for the living owner (swallowing it lost the card's whole chain,
	// silently and permanently). Only the watchdog's REPARKS — a write
	// into a launch column, where firing re-arms a fresh spend — stay
	// under the machine ReasonWatchdog.
	ReasonRunFinished = "run_finished"
	ReasonRunFailed   = "run_failed"
	// ReasonUnlaunchable is the dispatcher returning a card it claimed but
	// could not launch (no bot, an unresolvable bot, malformed launch
	// context) to the column it took it from. Nothing ran, so nothing
	// about the card's work was judged: machine provenance — a
	// subscription armed on that column must not re-fire on the return,
	// an audit surface must not sign it with the assignee's name, and an
	// external roadmap must not read it as a move.
	ReasonUnlaunchable = "unlaunchable"
)

// IsMachineReason reports whether a board-event `reason` names iterion
// acting on the board by itself (a watchdog repair, a schema migration).
// The set is ENUMERATED, never derived from "any reason is machine":
// a descriptive reason like ReasonUnblocked must keep firing the
// triggers it fired before it existed — deriving the predicate from the
// field's mere presence silently disarmed them.
func IsMachineReason(reason string) bool {
	switch reason {
	case ReasonWatchdog, ReasonStateRename, ReasonStateDelete, ReasonFieldRename, ReasonFieldDelete, ReasonUnlaunchable:
		return true
	}
	return false
}

// LaunchStateLister is the optional capability naming the states a card
// is DISPATCHED FROM. The claim watchdog needs it to tell two situations
// apart that look identical from the card alone: a card an operator
// deliberately moved (honour it), and a card whose best-effort move into
// the running column never landed (file it, or the next tick launches a
// second run for work already delivered). A tracker that cannot answer
// leaves the watchdog conservative — it honours every move it sees.
type LaunchStateLister interface {
	LaunchStates() []string
}

// Errors returned by Tracker implementations. Callers should use
// errors.Is to discriminate.
var (
	// ErrNotFound is returned when an issue ID does not exist.
	ErrNotFound = errors.New("tracker: issue not found")

	// ErrNotSupported is returned by adapters that don't implement an
	// optional capability (e.g. Comment on the native tracker before
	// Markdown comments are wired).
	ErrNotSupported = errors.New("tracker: operation not supported")

	// ErrTransitionRejected is returned when UpdateState is asked to
	// move an issue to an unknown or invalid state.
	ErrTransitionRejected = errors.New("tracker: state transition rejected")

	// ErrClaimConflict is returned when Claim is called on an issue
	// already claimed by a different marker.
	ErrClaimConflict = errors.New("tracker: issue already claimed by another marker")
)

// ErrTerminalStateExit marks a refused exit from a Terminal:true board
// state — the sink guard. It WRAPS ErrTransitionRejected so existing
// errors.Is(…, ErrTransitionRejected) callers keep matching; operator
// surfaces additionally match this sentinel to fall back to the
// sanctioned Reopen.
var ErrTerminalStateExit = fmt.Errorf("%w: terminal state is a sink", ErrTransitionRejected)
