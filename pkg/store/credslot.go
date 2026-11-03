package store

import (
	"context"
	"errors"
	"time"
)

// CredSlotStore is the optional capability behind a credential's HARD
// concurrency cap (secrets.ApiKey.MaxConcurrentRuns).
//
// The walk's own count (CountAliveRunsWithCredFingerprint) is a reading of
// the run documents taken before the run is written: two publisher replicas
// launching at once both read "one short of the cap" and both admit, a
// pinned key skips the reading, and the refused-key restore undoes it on
// purpose. The ledger behind this interface is the run documents' ATOMIC
// index — one row per fingerprint naming the runs that hold a slot — and an
// admission is one conditional write that either adds the run while the row
// has room or changes nothing, so of N concurrent admissions on a key with K
// free slots exactly K succeed, whatever replica each ran on.
//
// The ledger never becomes a second truth. A slot is released by what
// already releases one today — the run going terminal, parking, pausing, or
// re-stamping onto another credential — and a refusal re-reads the row's
// holders against the run documents before it is final
// (CredSlotHolderStale): a holder whose run no longer spends the credential
// is evicted and the admission retried once. A model-idle run
// (Run.LLMIdleSince) KEEPS its slot: a slot lent out while the run executes
// tool-only work has no path back when its next model node starts — nothing
// in the runner can wait for the ledger at a node start — so a cap that
// released it would admit one run past the cap on every wake. The marker
// stays observability. The one release a caller performs itself is the undo
// of an admission whose launch or resume provably never handed the run on
// (ReleaseCredSlot): an attempt whose queue message may have landed keeps
// its slots, and the reconcile frees them once its run document stops
// spending.
//
// A capability rather than part of RunStore, like RunRetryStore: the cloud
// publisher is its only caller, and a decorator that does not carry it must
// not hide it (AsCredSlotStore looks through Unwrapper).
type CredSlotStore interface {
	// ReserveCredSlot admits runID to hold one of max slots of fingerprint.
	// Granted is false when the row is full of runs that still spend the
	// credential; Held reports how many hold a slot after the call either
	// way. A run already holding a slot is admitted again in place, without
	// taking a second one — a resume's re-resolution reserves what the run
	// holds — while the row is within max: under a cap lowered below its
	// holders, a holder's re-admission waits like a newcomer's until enough
	// of the others stop spending. Every admission moves the holder's
	// instant forward, so an eviction judged before it misses the entry.
	// max <= 0 is a caller error: an uncapped key reserves nothing.
	ReserveCredSlot(ctx context.Context, fingerprint, runID string, max int) (CredSlotAdmission, error)
	// ReleaseCredSlot gives back the slot runID holds under fingerprint, if
	// any: the undo of an admission whose launch or resume did not land.
	ReleaseCredSlot(ctx context.Context, fingerprint, runID string) error
}

// CredSlotAdmission is what ReserveCredSlot decided.
type CredSlotAdmission struct {
	Granted bool
	Held    int
}

// AsCredSlotStore returns s as CredSlotStore when the backend hosts the
// ledger, or nil otherwise. Callers MUST nil-check.
func AsCredSlotStore(s RunStore) CredSlotStore { return capability[CredSlotStore](s) }

// CredSlotGrace is how long a holder whose run document does not exist keeps
// its slot. A launch reserves BEFORE it persists the run (the document is
// written once, in the status it will have), so a reconcile on another
// replica that lands between the two writes must not take the fresh holder
// for a deleted run. Past the grace the document is not coming — the launch
// died between its two writes — and the slot is given back.
const CredSlotGrace = 2 * time.Minute

// SpendsCredential reports whether the run holds a slot of the credential
// with this fingerprint right now: alive (RunStatus.HoldsCredentialSlot) and
// stamped with the fingerprint. Model-idle (LLMIdleSince) releases nothing
// — the ledger's rule above. The one predicate the alive-run count, the
// ledger's reconcile and the filesystem twin read, so none can answer
// differently from the others; the Mongo count's query filter is its
// mirror, witnessed by the CredFingerprintMeter conformance row.
func (r *Run) SpendsCredential(fingerprint string) bool {
	if r == nil || fingerprint == "" || !r.Status.HoldsCredentialSlot() {
		return false
	}
	for _, fp := range r.CredFingerprints {
		if fp == fingerprint {
			return true
		}
	}
	return false
}

// CredSlotHolderStale decides whether a ledger holder may be evicted, from
// what loading its run returned: a run that exists and no longer spends the
// credential, or a run that does not exist (never persisted, or deleted) and
// was reserved longer than CredSlotGrace ago. Any other load error keeps the
// holder — a store that cannot answer must not hand out a slot on a guess.
// Shared by both twins so they evict on one rule.
func CredSlotHolderStale(run *Run, loadErr error, reservedAt, now time.Time, fingerprint string) bool {
	switch {
	case loadErr == nil:
		return !run.SpendsCredential(fingerprint)
	case errors.Is(loadErr, ErrRunNotFound), errors.Is(loadErr, ErrRunDeleted):
		return now.Sub(reservedAt) > CredSlotGrace
	default:
		return false
	}
}
