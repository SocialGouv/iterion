package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"time"
)

// OutputCorrectionUsage is marginal usage, already normalized by the producer
// (session cost maxima are resolved before constructing a receipt).
type OutputCorrectionUsage struct {
	Tokens          int     `json:"tokens,omitempty" bson:"tokens,omitempty"`
	CostUSD         float64 `json:"cost_usd,omitempty" bson:"cost_usd,omitempty"`
	UnreportedCalls int     `json:"unreported_calls,omitempty" bson:"unreported_calls,omitempty"`
}

// OutputCorrectionCause is a versioned, allowlisted control cause. It contains
// no provider body, secret, task or session. Runtime owns encode/rehydration.
type OutputCorrectionCause struct {
	Version      int       `json:"version" bson:"version"`
	Kind         string    `json:"kind" bson:"kind"`
	Provider     string    `json:"provider,omitempty" bson:"provider,omitempty"`
	WindowKind   string    `json:"window_kind,omitempty" bson:"window_kind,omitempty"`
	ResetAt      time.Time `json:"reset_at,omitempty" bson:"reset_at,omitempty"`
	SelfImposed  bool      `json:"self_imposed,omitempty" bson:"self_imposed,omitempty"`
	NodeID       string    `json:"node_id,omitempty" bson:"node_id,omitempty"`
	TimeoutNanos int64     `json:"timeout_nanos,omitempty" bson:"timeout_nanos,omitempty"`
}

type OutputCorrectionReceipt struct {
	Projection  *UsageProjection      `json:"projection,omitempty" bson:"projection,omitempty"`
	Observation *UsageObservation     `json:"observation,omitempty" bson:"observation,omitempty"`
	Usage       OutputCorrectionUsage `json:"usage" bson:"usage"`
}

// A reservation exists before the paid call. The receipt can arrive after this
// epoch has stopped, but never grants another call or a verdict.
type OutputCorrectionReservation struct {
	// LogicalInvocationID identifies graph progress across distinct paid retry reservations.
	LogicalInvocationID string                   `json:"logical_invocation_id,omitempty" bson:"logical_invocation_id,omitempty"`
	Origin              *UsageOrigin             `json:"origin,omitempty" bson:"origin,omitempty"`
	InitialUsageID      string                   `json:"initial_usage_id,omitempty" bson:"initial_usage_id,omitempty"`
	ID                  string                   `json:"id" bson:"id"`
	ExecutionEpoch      string                   `json:"execution_epoch" bson:"execution_epoch"`
	ScopeKey            string                   `json:"scope_key" bson:"scope_key"`
	StartedAt           time.Time                `json:"started_at" bson:"started_at"`
	Receipt             *OutputCorrectionReceipt `json:"receipt,omitempty" bson:"receipt,omitempty"`
}

// OutputCorrectionReceiptTarget identifies the reservation that paid for a call.
// Capture it before the call and retain it with the result: reconstructing its
// epoch from the current run would let an old result credit a recreated run.
// The reservation epoch remains valid after its owner stops or is replaced.
type OutputCorrectionReceiptTarget struct {
	Kind           string `json:"kind,omitempty" bson:"kind,omitempty"`
	LedgerKey      string `json:"ledger_key" bson:"ledger_key"`
	Generation     string `json:"generation" bson:"generation"`
	ReservationID  string `json:"reservation_id" bson:"reservation_id"`
	ExecutionEpoch string `json:"execution_epoch" bson:"execution_epoch"`
}

func (r OutputCorrectionReservation) ReceiptTarget(key, generation string) OutputCorrectionReceiptTarget {
	return OutputCorrectionReceiptTarget{LedgerKey: key, Generation: generation, ReservationID: r.ID, ExecutionEpoch: r.ExecutionEpoch}
}

type OutputCorrectionExpected struct {
	Absent     bool
	Generation string
	Revision   int64
	Attempts   int
	Status     string
}

func ExpectOutputCorrection(e OutputCorrectionEpisode) OutputCorrectionExpected {
	return OutputCorrectionExpected{Generation: e.Generation, Revision: e.Revision, Attempts: e.Attempts, Status: e.Status}
}

// OutputCorrectionCASStore is separate from the legacy setter capability.
// Merely adding persistence must not enable a partial producer handoff.
type OutputCorrectionCASStore interface {
	RunExecutionStore
	SetRunOutputCorrectionIf(context.Context, string, RunExecutionAuthority, string, OutputCorrectionExpected, OutputCorrectionEpisode) (bool, error)
	RecordOutputCorrectionReceipt(context.Context, string, OutputCorrectionReceiptTarget, OutputCorrectionReceipt) (bool, error)
}

func AsOutputCorrectionCASStore(s RunStore) OutputCorrectionCASStore {
	return capability[OutputCorrectionCASStore](s)
}

var ErrInvalidOutputCorrection = errors.New("invalid output correction transition")

func invalidCorrection(detail string) error {
	return fmt.Errorf("%w: %s", ErrInvalidOutputCorrection, detail)
}

// ApplyOutputCorrectionDecision is shared by the FS and Mongo transactions.
// It is pure with respect to I/O; its caller must CAS the exact Run version.
func ApplyOutputCorrectionDecision(r *Run, authority RunExecutionAuthority, key string, expected OutputCorrectionExpected, next OutputCorrectionEpisode) (bool, error) {
	next = CloneOutputCorrectionEpisode(next)
	if key == "" {
		return false, invalidCorrection("empty ledger key")
	}
	if !authority.Matches(r) {
		return false, nil
	}
	current, exists := r.OutputCorrections[key]
	if expected.Absent {
		if exists {
			return false, nil
		}
	} else if !exists || ExpectOutputCorrection(current) != expected {
		return false, nil
	}
	if next.Generation == "" || next.Revision <= 0 || current.Revision == math.MaxInt64 || next.Revision != current.Revision+1 {
		return false, invalidCorrection("generation/revision")
	}
	if !exists || current.Generation == "" {
		for _, retired := range r.OutputCorrectionHistory {
			if retired.LedgerKey == key && retired.Generation == next.Generation {
				return false, invalidCorrection("generation already retired")
			}
		}
	}
	if exists && current.Generation != "" && next.Generation != current.Generation {
		return false, invalidCorrection("generation replacement requires retirement")
	}
	switch next.Status {
	case "active", "succeeded", "exhausted", "unchanged", "spend_blocked":
	default:
		return false, invalidCorrection("unknown status")
	}
	if next.Budget < 0 || next.Attempts < 0 {
		return false, invalidCorrection("negative bound")
	}
	if !exists && (next.Attempts != 0 || len(next.Reservations) != 0 || next.Status != "active") {
		return false, invalidCorrection("new episode is not empty and active")
	}
	if next.Cause != nil {
		if next.Cause.Version != 1 {
			return false, invalidCorrection("unknown cause version")
		}
		switch next.Cause.Kind {
		case "ambiguous", "auth", "usage_window", "node_deadline":
		default:
			return false, invalidCorrection("unknown cause kind")
		}
	}
	// Receipts and aggregate usage are store-owned. A stale verdict cannot
	// overwrite a late receipt even when its other declarative fields match.
	if next.Usage != current.Usage {
		return false, invalidCorrection("decision cannot change usage")
	}
	n := len(current.Reservations)
	if len(next.Reservations) < n || len(next.Reservations) > n+1 {
		return false, invalidCorrection("reservation count")
	}
	for i := range current.Reservations {
		if !sameCorrectionReservation(current.Reservations[i], next.Reservations[i]) {
			return false, invalidCorrection("decision changed existing reservation")
		}
	}
	if len(next.Reservations) == n+1 {
		if !exists || current.Status != "active" || next.Status != "active" || current.Attempts == math.MaxInt || next.Attempts != current.Attempts+1 || next.Attempts > next.Budget {
			return false, invalidCorrection("reservation outside active budget")
		}
		added := next.Reservations[n]
		if added.Origin != nil {
			return false, invalidCorrection("round cannot redefine usage origin")
		}
		if added.InitialUsageID != "" {
			initial, exists := r.InitialUsageReservations[added.InitialUsageID]
			if !exists || initial.Receipt == nil || initial.ExecutionEpoch != added.ExecutionEpoch || initial.ScopeKey != added.ScopeKey {
				return false, invalidCorrection("round lacks settled initial usage identity")
			}
		}
		if added.ID == "" || added.ScopeKey == "" || added.StartedAt.IsZero() || added.ExecutionEpoch != authority.Epoch || added.Receipt != nil {
			return false, invalidCorrection("invalid reservation")
		}
		for _, prior := range current.Reservations {
			if prior.ID == added.ID {
				return false, invalidCorrection("duplicate reservation")
			}
		}
	} else if next.Attempts != current.Attempts {
		return false, invalidCorrection("attempt without reservation")
	}
	if next.RetiredAt != nil || next.RetiredReason != "" {
		return false, invalidCorrection("decision cannot retire episode")
	}
	// Canonical wire precision is BSON's millisecond. Copy first: callers may
	// retain the submitted reservation to construct the next decision.
	next.Reservations = append([]OutputCorrectionReservation(nil), next.Reservations...)
	for i := range next.Reservations {
		next.Reservations[i].StartedAt = canonicalCorrectionTime(next.Reservations[i].StartedAt)
	}
	next.LedgerKey = key
	if r.OutputCorrections == nil {
		r.OutputCorrections = make(map[string]OutputCorrectionEpisode)
	}
	r.OutputCorrections[key] = next
	return true, nil
}

func canonicalCorrectionTime(t time.Time) time.Time { return t.UTC().Truncate(time.Millisecond) }

func sameCorrectionReservation(a, b OutputCorrectionReservation) bool {
	receiptEqual := a.Receipt == nil && b.Receipt == nil || a.Receipt != nil && b.Receipt != nil && reflect.DeepEqual(a.Receipt, b.Receipt)
	return reflect.DeepEqual(a.Origin, b.Origin) && a.ID == b.ID && a.InitialUsageID == b.InitialUsageID && a.LogicalInvocationID == b.LogicalInvocationID && a.ExecutionEpoch == b.ExecutionEpoch && a.ScopeKey == b.ScopeKey &&
		canonicalCorrectionTime(a.StartedAt).Equal(canonicalCorrectionTime(b.StartedAt)) && receiptEqual
}

func validCorrectionUsage(u OutputCorrectionUsage) bool {
	return u.Tokens >= 0 && u.UnreportedCalls >= 0 && u.CostUSD >= 0 && !math.IsNaN(u.CostUSD) && !math.IsInf(u.CostUSD, 0)
}

// ApplyOutputCorrectionReceipt changes accounting only, on a recognized current
// or retired generation and its original reservation epoch. Duplicate receipts
// are no-ops only after this identity check; contradictions fail. The active
// run epoch is deliberately irrelevant to a late receipt for retained history.
func ApplyOutputCorrectionReceipt(r *Run, target OutputCorrectionReceiptTarget, receipt OutputCorrectionReceipt) (bool, error) {
	var observationErr error
	receipt, observationErr = normalizeObservationReceipt(receipt)
	if observationErr != nil {
		return false, observationErr
	}
	if target.Kind == "initial" {
		return applyInitialUsageReceipt(r, target, receipt)
	}
	if target.Kind != "" && target.Kind != "correction" {
		return false, invalidCorrection("unknown receipt kind")
	}
	key, generation, reservationID := target.LedgerKey, target.Generation, target.ReservationID
	if key == "" || generation == "" || reservationID == "" || target.ExecutionEpoch == "" || !validCorrectionUsage(receipt.Usage) {
		return false, invalidCorrection("invalid receipt")
	}
	current, exists := r.OutputCorrections[key]
	historyIndex := -1
	if !exists || current.Generation != generation {
		found := false
		for i, e := range r.OutputCorrectionHistory {
			if e.LedgerKey == key && e.Generation == generation {
				current, historyIndex, found = e, i, true
				break
			}
		}
		if !found {
			return false, invalidCorrection("receipt generation is unknown")
		}
	}
	for i := range current.Reservations {
		if current.Reservations[i].ID != reservationID {
			continue
		}
		if current.Reservations[i].ExecutionEpoch != target.ExecutionEpoch {
			return false, invalidCorrection("receipt reservation epoch differs")
		}
		if old := current.Reservations[i].Receipt; old != nil {
			if sameIncomingReceipt(*old, receipt) {
				return false, nil
			}
			return false, invalidCorrection("contradictory receipt")
		}
		var marks map[string]float64
		receipt, marks, observationErr = observedReceipt(r, receipt, current.Reservations[i])
		if observationErr != nil {
			return false, observationErr
		}
		u := current.Usage
		if current.Revision == math.MaxInt64 || receipt.Usage.Tokens > math.MaxInt-u.Tokens || receipt.Usage.UnreportedCalls > math.MaxInt-u.UnreportedCalls {
			return false, invalidCorrection("usage overflow")
		}
		u.Tokens += receipt.Usage.Tokens
		u.UnreportedCalls += receipt.Usage.UnreportedCalls
		u.CostUSD += receipt.Usage.CostUSD
		if !validCorrectionUsage(u) {
			return false, invalidCorrection("usage overflow")
		}
		// Copy the slice so a rejected/uncommitted transaction cannot mutate an
		// episode held by its caller through a shared backing array.
		current.Reservations = append([]OutputCorrectionReservation(nil), current.Reservations...)
		current.Reservations[i].Receipt = &receipt
		current.Usage = u
		current.Revision++
		if historyIndex >= 0 {
			r.OutputCorrectionHistory[historyIndex] = current
		} else {
			r.OutputCorrections[key] = current
		}
		if marks != nil {
			r.UsageSessionMarks = marks
		}
		return true, nil
	}
	return false, invalidCorrection("receipt reservation is unknown")
}

func (s *FilesystemRunStore) SetRunOutputCorrectionIf(ctx context.Context, id string, a RunExecutionAuthority, key string, expected OutputCorrectionExpected, next OutputCorrectionEpisode) (bool, error) {
	next = CloneOutputCorrectionEpisode(next)
	r, unlock, err := s.lockExistingRunMetadata(ctx, id)
	if err != nil {
		return false, err
	}
	defer unlock()
	preimageEpoch := r.ExecutionEpoch
	ok, err := ApplyOutputCorrectionDecision(r, a, key, expected, next)
	if !ok || err != nil {
		return ok, err
	}
	r.UpdatedAt = time.Now().UTC()
	if err := s.writeRunLockedContext(ctx, r, false, &preimageEpoch); err != nil {
		return false, err
	}
	return true, nil
}

func (s *FilesystemRunStore) RecordOutputCorrectionReceipt(ctx context.Context, id string, target OutputCorrectionReceiptTarget, receipt OutputCorrectionReceipt) (bool, error) {
	var freezeErr error
	receipt, freezeErr = FreezeOutputCorrectionReceipt(receipt)
	if freezeErr != nil {
		return false, freezeErr
	}
	r, unlock, err := s.lockExistingRunMetadata(ctx, id)
	if err != nil {
		return false, err
	}
	defer unlock()
	preimageEpoch := r.ExecutionEpoch
	ok, err := ApplyOutputCorrectionReceipt(r, target, receipt)
	if !ok || err != nil {
		return ok, err
	}
	r.UpdatedAt = time.Now().UTC()
	if err := s.writeRunLockedContext(ctx, r, false, &preimageEpoch); err != nil {
		return false, err
	}
	return true, nil
}
