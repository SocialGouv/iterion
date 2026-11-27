package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

// The capacity transit (ADR-167 C1): a run handed back to the queue for a
// redelivery keeps the capacity its last admission reserved until the
// redelivery resumes it or the hold expires.
//
// A delivery that ends on a nak the queue will redeliver leaves its run
// failed_resumable with continuation redelivery_pending. Before this hold such
// a run held nothing: a reconcile on another replica evicted its ledger
// entries, a newcomer took the slot, and the redelivery resumed onto a full
// key — the cap exceeded by one for as long as both ran. In transit the run is
// a holder for every reading of the ledger — the count, the eviction, the
// adoption — until ContinuationUntil, an instant its writer stamps at its own
// clock (the runner's), so no reader needs the TTL.
//
// The runner that picks the redelivery up proves it still holds its places
// before it resumes: it re-asserts each ledger entry its message's admission
// reserved (CredSlotStore.ReassertCredSlot), matched on that admission's
// identity, and parks the run on CRED_SLOT_WAIT when one is gone. It never
// acquires a slot: the retry sweeper's re-admission does.

// RedeliveryHoldTTLEnv names the runner setting of the transit's length
// (RedeliveryHoldTTLFromEnv). Servers never read it: the instant is stamped on
// the run by the runner that writes the transit.
const RedeliveryHoldTTLEnv = "ITERION_REDELIVERY_HOLD_TTL"

// DefaultRedeliveryHoldTTL is the transit's length when RedeliveryHoldTTLEnv
// is unset.
const DefaultRedeliveryHoldTTL = 10 * time.Minute

// RedeliveryHoldTTLFloor is the shortest transit that outlasts the runner's
// own re-offers: the longest delayed nak a delivery hands back (3 minutes, a
// sandbox that was not placed) plus the floor under which a running document
// is not adopted (4 minutes). A shorter hold is honoured, with a warning: the
// run then loses its places while a redelivery that will resume it is still
// on its way.
const RedeliveryHoldTTLFloor = 7 * time.Minute

// ParseRedeliveryHoldTTL reads the transit's length from a setting's raw value
// (set false: the setting is absent, the default holds). The value is a Go
// duration ("10m", "90s") and must be strictly positive; anything else is an
// error, never a fallback. A value below RedeliveryHoldTTLFloor is accepted
// and returned with a warning that says why it is short.
func ParseRedeliveryHoldTTL(raw string, set bool) (time.Duration, string, error) {
	if !set {
		return DefaultRedeliveryHoldTTL, "", nil
	}
	ttl, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, "", fmt.Errorf("%s=%q: not a duration (e.g. 10m): %w", RedeliveryHoldTTLEnv, raw, err)
	}
	if ttl <= 0 {
		return 0, "", fmt.Errorf("%s=%q: the hold must be strictly positive", RedeliveryHoldTTLEnv, raw)
	}
	if ttl < RedeliveryHoldTTLFloor {
		return ttl, fmt.Sprintf("%s=%s is below %s (the longest delayed redelivery, 3m, plus the 4m running-adoption floor): a run handed back to the queue can lose its capacity places before its redelivery resumes it, and then parks on CRED_SLOT_WAIT", RedeliveryHoldTTLEnv, ttl, RedeliveryHoldTTLFloor), nil
	}
	return ttl, "", nil
}

// RedeliveryHoldTTLFromEnv is ParseRedeliveryHoldTTL on the process
// environment.
func RedeliveryHoldTTLFromEnv() (time.Duration, string, error) {
	raw, set := os.LookupEnv(RedeliveryHoldTTLEnv)
	return ParseRedeliveryHoldTTL(raw, set)
}

// TransitInstant is an instant as every store keeps it: UTC, to the
// millisecond. A transit's until is compared for equality by the writes that
// end or refresh it, so its writer keeps the value it wrote in this form.
func TransitInstant(t time.Time) time.Time { return t.UTC().Truncate(time.Millisecond) }

// InCapacityTransit reports whether the run holds its capacity places while
// the queue redelivers it: failed_resumable, continuation redelivery_pending,
// and its ContinuationUntil still ahead of now. Every reading compares with
// its own now; the instant is the writer's.
func (r *Run) InCapacityTransit(now time.Time) bool {
	return r != nil && r.Status == RunStatusFailedResumable &&
		r.ContinuationState == ContinuationRedeliveryPending &&
		r.ContinuationUntil != nil && r.ContinuationUntil.After(now)
}

// HoldsCapacity reports whether the run holds the credentials and capacity
// subjects its stamp names right now: alive (RunStatus.HoldsCredentialSlot) or
// in transit (InCapacityTransit). The one holder predicate; the Mongo filter
// holdsCapacityBranches is its mirror.
func (r *Run) HoldsCapacity(now time.Time) bool {
	return r != nil && (r.Status.HoldsCredentialSlot() || r.InCapacityTransit(now))
}

// StampsCapacity reports whether the run's stamp names subject — a credential
// fingerprint (CredFingerprints) or a capacity subject (CapacitySubjects) —
// whatever its status.
func (r *Run) StampsCapacity(subject string) bool {
	if r == nil || subject == "" {
		return false
	}
	return slices.Contains(r.CredFingerprints, subject) || slices.Contains(r.CapacitySubjects, subject)
}

// CredSlotProbeInterval paces how often a parked run asks for a slot again:
// the first ask a park arms, and every free deferral the retry sweeper grants
// after a refused resume. A cap lapses when a run ends, not at an instant
// anyone can name, so the wait is a poll: short enough that a freed slot is
// taken within minutes, long enough that a key busy for hours is not asked on
// every sweep tick. The run's own retry policy bounds the whole wait
// (max_wait).
const CredSlotProbeInterval = 2 * time.Minute

// CapacityHold is one place an admission reserved for a run: the ledger row's
// subject and the identity of the entry that admission wrote
// (CredSlotAdmission.At, kept by the entry as its admitted_at). A queue
// message carries its admission's holds (queue.CapacityAdmission).
type CapacityHold struct {
	Subject string    `json:"subject"`
	Token   time.Time `json:"token"`
}

// CapacityTransit is the transit a runner begins or refreshes
// (CapacityTransitStore.BeginCapacityTransit).
type CapacityTransit struct {
	// Until is the end of the hold, at the writer's clock (TransitInstant).
	Until time.Time
	// ExactQueuedAt, when set, fences the write to the attempt that marker
	// names, as RunOutcomeMeta.ExactQueuedAt does.
	ExactQueuedAt time.Time
	// ExpectUntil, when set, refreshes only the transit that ended at that
	// instant — the one this writer began.
	ExpectUntil time.Time
}

// CapacityTransitStart is what BeginCapacityTransit did.
type CapacityTransitStart struct {
	// Started: the run was failed_resumable with no armed retry (and with the
	// expected transit, the expected attempt) and is now in transit.
	Started bool
	// Prior is the continuation the write replaced, what an end of the
	// transit that resumed nothing puts back.
	Prior ContinuationState
}

// CapacityTransitStore is the optional capability a runner holds a run's
// places through between two deliveries. Both twins host it.
type CapacityTransitStore interface {
	// BeginCapacityTransit puts a failed_resumable run whose continuation is
	// anything but retry_armed in transit until t.Until, in one conditional
	// write: continuation_state redelivery_pending, continuation_until, the
	// update time and the version. An armed retry is the sweeper's: a
	// redelivery does not take it over. Not matched is Started false with
	// nothing written.
	BeginCapacityTransit(ctx context.Context, runID string, t CapacityTransit) (CapacityTransitStart, error)
	// EndCapacityTransit ends the transit that runs until `until`, while the
	// run is still failed_resumable in it: the continuation becomes restore
	// (removed when empty) and the instant is removed. A claim, a park or a
	// later transit is not matched, and false is returned.
	EndCapacityTransit(ctx context.Context, runID string, until time.Time, restore ContinuationState) (bool, error)
}

// AsCapacityTransitStore returns s as CapacityTransitStore when the backend
// hosts it, or nil otherwise. Callers MUST nil-check.
func AsCapacityTransitStore(s RunStore) CapacityTransitStore {
	return capability[CapacityTransitStore](s)
}

// ReassertedHold is a hold whose entry a re-assertion moved: its subject, its
// admission's token, and the entry's new instant — the one a release names.
type ReassertedHold struct {
	CapacityHold
	At time.Time
}

// CapacityTransitEntry is what EnterCapacityTransit found.
type CapacityTransitEntry struct {
	// Started: the transit write matched (CapacityTransitStart.Started).
	// Nothing was re-asserted when it did not.
	Started bool
	Prior   ContinuationState
	// Held are the holds re-asserted, in message order, with their entries'
	// new instants. Empty when one was lost.
	Held []ReassertedHold
	// Lost names the first hold whose entry the ledger no longer holds for
	// this admission; "" when every hold is held.
	Lost string
}

// capacityTransitStepSeam is the context key of WithCapacityTransitStepSeam.
type capacityTransitStepSeam struct{}

// WithCapacityTransitStepSeam returns ctx carrying fn, which
// EnterCapacityTransit calls between its transit write and its first
// re-assertion — the seam an interleaving proof drives another writer
// through. Production callers never set it.
func WithCapacityTransitStepSeam(ctx context.Context, fn func()) context.Context {
	return context.WithValue(ctx, capacityTransitStepSeam{}, fn)
}

// EnterCapacityTransit is the runner's entry into a redelivered run's
// transit, in the one order that keeps its places: the transit write first,
// so the run is a holder for every eviction judged from then on, and only
// then the re-assertion of each hold, which moves the entry an eviction
// judged before the write would pull. In the other order an eviction judged
// between the two writes sees a run that holds nothing and takes an entry the
// re-assertion just proved.
//
// A hold the ledger no longer holds for its admission stops the entry: the
// holds re-asserted before it are given back by their new instants, and Lost
// names it. A store error is returned as is, the re-asserted holds kept: the
// run stays in transit, and the next delivery re-asserts them by the same
// tokens. Nothing is re-asserted when the transit write did not match.
func EnterCapacityTransit(ctx context.Context, transit CapacityTransitStore, ledger CredSlotStore, runID string, holds []CapacityHold, t CapacityTransit) (CapacityTransitEntry, error) {
	if transit == nil || ledger == nil {
		return CapacityTransitEntry{}, fmt.Errorf("store: enter the capacity transit of run %s: the store hosts no transit or no ledger", runID)
	}
	start, err := transit.BeginCapacityTransit(ctx, runID, t)
	if err != nil {
		return CapacityTransitEntry{}, err
	}
	out := CapacityTransitEntry{Started: start.Started, Prior: start.Prior}
	if !start.Started {
		return out, nil
	}
	if seam, ok := ctx.Value(capacityTransitStepSeam{}).(func()); ok && seam != nil {
		seam()
	}
	held, lost, err := ReassertCapacityHolds(ctx, ledger, runID, holds)
	if err != nil {
		return out, err
	}
	out.Held, out.Lost = held, lost
	return out, nil
}

// ReassertCapacityHolds re-asserts each hold in order (ReassertCredSlot) and
// returns them with their entries' new instants. The first hold not held
// stops it: the holds already re-asserted are released by their new instants
// and its subject is returned. A store error returns what was re-asserted
// before it, unreleased.
func ReassertCapacityHolds(ctx context.Context, ledger CredSlotStore, runID string, holds []CapacityHold) ([]ReassertedHold, string, error) {
	held := make([]ReassertedHold, 0, len(holds))
	for _, h := range holds {
		re, err := ledger.ReassertCredSlot(ctx, h.Subject, runID, h.Token)
		if err != nil {
			return held, "", fmt.Errorf("store: re-assert the %s hold of run %s: %w", h.Subject, runID, err)
		}
		if !re.Held {
			if rerr := ReleaseReassertedHolds(ctx, ledger, runID, held); rerr != nil {
				return nil, h.Subject, rerr
			}
			return nil, h.Subject, nil
		}
		held = append(held, ReassertedHold{CapacityHold: h, At: re.At})
	}
	return held, "", nil
}

// ReleaseReassertedHolds gives back each hold by its entry's newest instant,
// every one attempted; the errors are joined.
func ReleaseReassertedHolds(ctx context.Context, ledger CredSlotStore, runID string, held []ReassertedHold) error {
	var errs []error
	for _, h := range held {
		if err := ledger.ReleaseCredSlot(ctx, h.Subject, runID, h.At); err != nil {
			errs = append(errs, fmt.Errorf("store: release the %s hold of run %s: %w", h.Subject, runID, err))
		}
	}
	return errors.Join(errs...)
}

// ErrCapacityPlaceLost is a resume's refusal when a place its admission
// reserved is no longer held for it: the run parks on CRED_SLOT_WAIT and the
// retry sweeper asks for a slot again. Never retried by a redelivery.
var ErrCapacityPlaceLost = errors.New("capacity place lost before the resume")

// ErrCapacityLedgerUnavailable is a resume's refusal when the ledger could not
// say whether its places are held, or its transit expired before the claim:
// the delivery is re-offered after a delay.
var ErrCapacityLedgerUnavailable = errors.New("capacity ledger could not prove the resume's places")

// CapacityPark is a park on a capacity wait armed in one write
// (CapacityParker): the status, the failure, the retry the sweeper performs
// and its continuation, together, so no reader ever sees one without the
// others.
type CapacityPark struct {
	// From is the status parked: queued (a launch, fenced by ExactQueuedAt)
	// or failed_resumable (a run in the transit TransitUntil names).
	From          RunStatus
	ExactQueuedAt time.Time
	TransitUntil  time.Time
	Code          FailureCode
	Error         string
	// Reason is the retry's reason (RetryReasonCredSlot).
	Reason     string
	RetryAfter time.Time
	// Charge spends one attempt of the run's retry budget, and is refused
	// when the budget (MaxAttempts) is spent. A free park writes the retry
	// whatever the budget.
	Charge      bool
	MaxAttempts int
	// Frozen, when set, is the program the parked delivery executed
	// (FrozenProgram), written on the run root by the same write, its
	// QueuedAt the document's own attempt marker read by that write. Nil
	// removes any record the run carries: a park of a delivery that froze
	// nothing leaves no program to re-admit. Only a park from
	// failed_resumable carries one — a run parked from queued never started.
	Frozen *FrozenProgram
}

// Validate refuses a park its writer cannot fence (From and its fence), cannot
// arm (ValidateRetry), or a frozen program it cannot bind.
func (p CapacityPark) Validate() error {
	switch p.From {
	case RunStatusQueued:
		if p.ExactQueuedAt.IsZero() {
			return errors.New("store: capacity park from queued needs the attempt's queued_at")
		}
	case RunStatusFailedResumable:
		if p.TransitUntil.IsZero() {
			return errors.New("store: capacity park from failed_resumable needs the transit it ends")
		}
	default:
		return fmt.Errorf("store: capacity park from %q: only queued and failed_resumable park", p.From)
	}
	if p.Frozen != nil {
		if p.From != RunStatusFailedResumable {
			return fmt.Errorf("store: capacity park from %q carries a frozen program: only a started run's park (from failed_resumable) freezes one", p.From)
		}
		if !p.Frozen.Valid() {
			return fmt.Errorf("store: capacity park carries an incomplete frozen program (key=%q digest=%q hash=%q source=%q)", p.Frozen.Key, p.Frozen.Digest, p.Frozen.WorkflowHash, p.Frozen.Source)
		}
	}
	return p.ValidateRetry()
}

// ValidateRetry refuses a park whose retry cannot be armed: no code, reason or
// instant, or a charge with no budget to charge.
func (p CapacityPark) ValidateRetry() error {
	if p.Code == "" || p.Reason == "" || p.RetryAfter.IsZero() {
		return fmt.Errorf("store: capacity park needs a code, a retry reason and a retry instant (code=%q reason=%q)", p.Code, p.Reason)
	}
	if p.Charge && p.MaxAttempts <= 0 {
		return fmt.Errorf("store: charged capacity park with max_attempts=%d", p.MaxAttempts)
	}
	return nil
}

// ErrCapacityParkBudgetSpent is SaveParkedLaunchRun's refusal of a charged
// park on a run whose retry budget is spent.
var ErrCapacityParkBudgetSpent = errors.New("capacity park: the run's retry budget is spent")

// CapacityParker is the optional capability that parks a run on a capacity
// wait in one write. Mongo only, like RunRetryStore: no durable retry exists
// on the filesystem store, whose callers keep their unarmed wording.
type CapacityParker interface {
	// ParkRunOnCapacity parks the run p.From names, in one conditional write:
	// failed_resumable with p.Code and p.Error, the retry armed at
	// p.RetryAfter for p.Reason, continuation retry_armed, the transit
	// instant removed, and the frozen program p.Frozen names bound to the
	// document's queued_at (or any record removed when it names none). A run
	// that moved, a charged park over a spent budget, are not parked: parked
	// false, nothing written. attempt is the run's retry attempt count after
	// the write.
	ParkRunOnCapacity(ctx context.Context, runID string, p CapacityPark) (parked bool, attempt int, err error)
	// SaveParkedLaunchRun is SaveRun of a launch the admission parked: the
	// document written failed_resumable with its retry armed and continuation
	// retry_armed in the same write. A charged park on a spent budget returns
	// ErrCapacityParkBudgetSpent with nothing written.
	SaveParkedLaunchRun(ctx context.Context, r *Run, p CapacityPark) error
}

// AsCapacityParker returns s as CapacityParker when the backend hosts it, or
// nil otherwise. Callers MUST nil-check.
func AsCapacityParker(s RunStore) CapacityParker { return capability[CapacityParker](s) }
