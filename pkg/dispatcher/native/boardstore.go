package native

import (
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// BoardStore is the storage contract the board operations (boardops), the
// dispatcher tracker adapter, and the REST handlers operate against. The
// filesystem-backed *Store satisfies it; a cloud build can supply a
// Mongo-backed implementation of the SAME contract so the shared boardops and
// dispatcher run unchanged against either backend.
//
// The board domain types (Issue, Patch, ListFilter, Board, Event, LabelUsage)
// live in this package; a non-filesystem implementation imports them from
// here. They are plain JSON/BSON-friendly structs with no filesystem
// coupling, so this is types-only reuse, not behaviour.
type BoardStore interface {
	Board() *Board
	SetBoard(b *Board) error

	Create(in Issue) (*Issue, error)
	Get(id string) (*Issue, error)
	List(filter ListFilter) ([]*Issue, error)
	Update(id string, p Patch) (*Issue, error)
	SetState(id, newState string) (*Issue, error)
	// SetStateFrom is the CAS move for AUTOMATED writers (changed=false
	// when the state drifted); Reopen is the ONE sanctioned exit from a
	// Terminal:true state (operator surfaces only — see the guard in
	// state_guard.go).
	SetStateFrom(id, from, to string) (*Issue, bool, error)
	Reopen(id, toState string) (*Issue, error)
	Delete(id string) error
	// AdjustLabels is the RELATIVE label write (add_labels /
	// remove_labels): the delta applies to the card as it is, atomically
	// — one critical section on the FS twin, one conditional write on the
	// Mongo one. Update's absolute label list is the caller's intent and
	// re-arms a one-shot trigger label consumed after the caller's read;
	// this cannot. changed=false when nothing moved (no write, no event).
	// Mandatory, not optional: a backend without it would degrade the
	// board ops to the read-modify-write this exists to remove.
	AdjustLabels(id string, add, remove []string) (*Issue, bool, error)

	// Claim/Release are the dispatcher's per-issue claim (marker = the
	// dispatcher instance id). Claim stamps a persisted LEASE and returns
	// the ownership token (marker + fencing epoch) the Owned variants
	// below demand; RenewClaim is the heartbeat that keeps a live
	// worker's claim from expiring. These are MANDATORY contract methods,
	// not an optional capability: an optional lease would be silently
	// inert on the backend that forgets it, and the cloud board is
	// exactly where the reaper matters (the SetLastRun/Adapter regression
	// documents how a silent optional-interface miss plays out).
	Claim(id, marker string) (tracker.ClaimToken, error)
	RenewClaim(id string, tok tracker.ClaimToken) error
	Release(id, marker string) error
	// ReleaseOwned + the *Owned mutators are the fenced write family: a
	// CAS on (claim, claim_epoch) so a worker whose claim was stolen
	// finds every late write refused (tracker.ErrClaimConflict) instead
	// of clobbering the new owner's state.
	ReleaseOwned(id string, tok tracker.ClaimToken) error
	SetStateOwned(id, newState string, tok tracker.ClaimToken) (*Issue, error)
	// SetStateOwnedReason is SetStateOwned with an EXPLICIT provenance
	// overriding the marker-derived one — the watchdog's terminal filings
	// carry the run's own verdict (run_finished / run_failed), the
	// dispatcher's give-back of a card it could not launch carries the
	// machine ReasonUnlaunchable. The reason lands on the event AND on the
	// card (Issue.StateReason), on both twins.
	SetStateOwnedReason(id, newState string, tok tracker.ClaimToken, reason string) (*Issue, error)
	// SetStateOwnedFrom is SetStateOwned with a source-state precondition:
	// ONE CAS on (claim, claim_epoch, state == from). changed=false when
	// the state drifted (somebody moved the card while its owner was
	// deciding — nothing is written); ErrClaimConflict when the token no
	// longer owns the card. The finish worker's auto-transition needs both
	// halves in one write: fenced alone it overwrote an operator move that
	// landed between its state probe and its write, while the watchdog —
	// which judges on the CAS-observed state — would have honoured it.
	SetStateOwnedFrom(id, from, to string, tok tracker.ClaimToken) (*Issue, bool, error)
	SetLastRunOwned(id, runID, workdir string, tok tracker.ClaimToken) error
	SetAwaitingInputOwned(id string, v bool, tok tracker.ClaimToken) error
	SetGaveUpOwned(id string, g *GiveUp, tok tracker.ClaimToken) error
	// SetLaunchRefusalOwned writes (nil clears) the launch-refusal ledger
	// of a card the token owns — the dispatcher's own record, so fenced
	// like every owner write. Both twins clear it themselves when a run is
	// stamped (SetLastRun / SetLastRunOwned: a launch happened) and on
	// Reopen (an operator gesture).
	SetLaunchRefusalOwned(id string, r *LaunchRefusal, tok tracker.ClaimToken) error
	// The reaper half — MANDATORY for the same reason as the lease: an
	// optional watchdog is silently inert exactly where it matters (the
	// cloud). ListExpiredClaimCandidates never lists a legacy claim;
	// ReclaimExpired is a CAS TRANSFER (claim still prev + still
	// expired → new owner, epoch bumped), never a bare clear.
	ListExpiredClaimCandidates(cutoff time.Time, limit int) ([]tracker.ExpiredClaim, error)
	ReclaimExpired(id string, prev tracker.ClaimToken, marker string, cutoff time.Time) (tracker.ClaimToken, string, error)
	// SetLastRun records the run a dispatch spawned so a cross-restart
	// resume can find it (unfenced form — for writers acting on
	// UNCLAIMED cards, e.g. the parked-card reconcilers).
	SetLastRun(id, runID, workdir string) error
	// SetAwaitingInput denormalizes onto the issue whether its most recent
	// run parked awaiting human/operator input, so the board grid can badge
	// the card without a per-run fetch. A best-effort HINT (see Issue.AwaitingInput).
	SetAwaitingInput(id string, v bool) error
	// SetGaveUp records (nil clears) that the dispatcher filed this ticket
	// itself after exhausting its retry budget, so a reader can tell an
	// automatic give-up from an operator's own filing (see Issue.GaveUp).
	SetGaveUp(id string, g *GiveUp) error

	// AddComment appends a note to the issue's discussion thread and
	// returns the updated issue plus the created comment.
	AddComment(id, author, body string) (*Issue, *Comment, error)

	Resolve(prefix string) (string, error)
	ScanEvents(visit func(*Event) bool) error
	AggregateLabels() []LabelUsage
}

// UniqueTitleCreator is the optional interface for board backends that can
// assign a distinct card title atomically with the create write (so two
// concurrent creates of the same title cannot both land it — PR #193 M4).
// The filesystem store implements it; a backend that does not cleanly
// degrades to the caller's best-effort list-then-check.
type UniqueTitleCreator interface {
	// normalize, when non-nil, is applied to every candidate title inside the
	// critical section — including the "#N - " prefixed variants. It lets a
	// caller keep the result within a display budget: prefixing "#N - " onto an
	// already-max-length title would otherwise overflow it, so the caller passes
	// its own rune-aware truncator (the prefix survives because truncation keeps
	// the head). nil = titles are used verbatim.
	CreateUniqueTitle(in Issue, normalize func(string) string) (*Issue, error)
}

// AsUniqueTitleCreator returns s as UniqueTitleCreator when the backend
// supports the atomic-unique-title create, or nil otherwise.
func AsUniqueTitleCreator(s BoardStore) UniqueTitleCreator {
	if s == nil {
		return nil
	}
	u, _ := s.(UniqueTitleCreator)
	return u
}

// StateReasoner is the optional provenance-carrying state setter. The
// shared auto-promote uses it so BOTH twins emit the same descriptive
// reason (tracker.ReasonUnblocked) on the same gesture — without it the
// FS twin stamped and the Mongo twin did not, and the trigger spine read
// two different truths from one close. The conformance suite is what
// keeps a backend from silently degrading to the bare SetState.
type StateReasoner interface {
	SetStateWithReason(id, newState, reason string) (*Issue, error)
}

// LaunchClaimer is the optional interface for board backends that can
// atomically claim a Ready ticket for launch — a CAS StateReady →
// StateInProgress that reports whether THIS caller won (PR #193 M2). It
// closes the check-then-act window where a live dispatcher and the studio
// admission loop both pick the same Ready ticket, and it reads the CLAIM
// family too: the dispatcher wins a card with the claim and moves it to
// in_progress afterwards, so a claimed card can legally sit in Ready while
// its run is already launching. Both twins implement it (the filesystem
// store here, boardmongo for the cloud — pinned by the shared conformance
// suite); a backend without it degrades the caller to a best-effort
// SetState, which under the claim lease is a second launch authority.
type LaunchClaimer interface {
	ClaimForLaunch(id string) (*Issue, bool, error)
}

// AsLaunchClaimer returns s as LaunchClaimer when the backend supports the
// atomic launch claim, or nil otherwise.
func AsLaunchClaimer(s BoardStore) LaunchClaimer {
	if s == nil {
		return nil
	}
	c, _ := s.(LaunchClaimer)
	return c
}

// Compile-time assertion that the filesystem store satisfies the contract.
var _ BoardStore = (*Store)(nil)
var _ UniqueTitleCreator = (*Store)(nil)
var _ LaunchClaimer = (*Store)(nil)
var _ StateReasoner = (*Store)(nil)

// Compile-time guarantees: the filesystem store is a full board backend.
var (
	_ BoardStore = (*Store)(nil)
	_ BoardAdmin = (*Store)(nil)
)
