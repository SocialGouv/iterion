package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Prototype contract only: the caller submits Observation and zero Usage.
// The store derives Usage under the same CAS that advances session maxima.
type UsageObservation struct {
	CollectionID    string                   `json:"collection_id,omitempty" bson:"collection_id,omitempty"`
	Components      []UsageComponent         `json:"components,omitempty" bson:"components,omitempty"`
	Version         int                      `json:"version" bson:"version"`
	CountIteration  bool                     `json:"count_iteration,omitempty" bson:"count_iteration,omitempty"`
	Tokens          int                      `json:"tokens,omitempty" bson:"tokens,omitempty"`
	UnpricedTokens  int                      `json:"unpriced_tokens,omitempty" bson:"unpriced_tokens,omitempty"`
	UnreportedCalls int                      `json:"unreported_calls,omitempty" bson:"unreported_calls,omitempty"`
	OtherCostUSD    float64                  `json:"other_cost_usd,omitempty" bson:"other_cost_usd,omitempty"`
	Sessions        []SessionCostObservation `json:"sessions,omitempty" bson:"sessions,omitempty"`
}
type SessionCostObservation struct {
	Backend     string  `json:"backend" bson:"backend"`
	SessionID   string  `json:"session_id" bson:"session_id"`
	Fingerprint string  `json:"fingerprint,omitempty" bson:"fingerprint,omitempty"`
	CostUSD     float64 `json:"cost_usd" bson:"cost_usd"`
}

func sessionCostKey(s SessionCostObservation) string {
	h := sha256.New()
	var n [8]byte
	for _, v := range []string{s.Backend, s.SessionID, s.Fingerprint} {
		binary.BigEndian.PutUint64(n[:], uint64(len(v)))
		_, _ = h.Write(n[:])
		_, _ = h.Write([]byte(v))
	}
	return hex.EncodeToString(h.Sum(nil))
}
func normalizeObservationReceipt(in OutputCorrectionReceipt) (OutputCorrectionReceipt, error) {
	if in.Projection != nil {
		return in, invalidCorrection("caller must not supply derived projection")
	}
	if in.Observation == nil {
		return in, nil
	}
	if in.Usage != (OutputCorrectionUsage{}) {
		return in, invalidCorrection("observed receipt must not supply derived usage")
	}
	obs := *in.Observation
	obs.Sessions = append([]SessionCostObservation(nil), obs.Sessions...)
	obs.Components = CloneUsageComponents(obs.Components)
	if (obs.Version != 1 && obs.Version != 2) || obs.Tokens < 0 || obs.UnpricedTokens < 0 || obs.UnpricedTokens > obs.Tokens || obs.UnreportedCalls < 0 || obs.OtherCostUSD < 0 || math.IsNaN(obs.OtherCostUSD) || math.IsInf(obs.OtherCostUSD, 0) {
		return in, invalidCorrection("invalid usage observation")
	}
	slices.SortFunc(obs.Sessions, func(a, b SessionCostObservation) int { return strings.Compare(sessionCostKey(a), sessionCostKey(b)) })
	for i, s := range obs.Sessions {
		if s.Backend == "" || s.SessionID == "" || !utf8.ValidString(s.Backend) || !utf8.ValidString(s.SessionID) || !utf8.ValidString(s.Fingerprint) || s.CostUSD < 0 || math.IsNaN(s.CostUSD) || math.IsInf(s.CostUSD, 0) {
			return in, invalidCorrection("invalid session observation")
		}
		if i > 0 && sessionCostKey(obs.Sessions[i-1]) == sessionCostKey(s) {
			return in, invalidCorrection("duplicate session observation")
		}
	}
	if obs.Version == 1 {
		if len(obs.Components) > 0 || obs.CollectionID != "" {
			return in, invalidCorrection("v1 cannot carry route components")
		}
		obs.Components = nil
	} else {
		summary, err := SummarizeUsageComponents(obs.CollectionID, obs.Components)
		if err != nil {
			return in, err
		}
		if obs.Tokens != summary.Tokens || obs.UnpricedTokens != summary.UnpricedTokens || obs.UnreportedCalls != summary.UnreportedCalls || obs.OtherCostUSD != summary.OtherCostUSD || !reflect.DeepEqual(obs.Sessions, summary.Sessions) {
			return in, invalidCorrection("component totals differ from observation")
		}
		summary.CountIteration = obs.CountIteration
		obs = summary
	}
	in.Observation = &obs
	return in, nil
}
func sameIncomingReceipt(old, next OutputCorrectionReceipt) bool {
	if old.Observation != nil || next.Observation != nil {
		return old.Observation != nil && next.Observation != nil && reflect.DeepEqual(old.Observation, next.Observation)
	}
	return old.Usage == next.Usage
}
func observedReceipt(r *Run, in OutputCorrectionReceipt, res OutputCorrectionReservation) (OutputCorrectionReceipt, map[string]float64, error) {
	if in.Observation == nil {
		return in, nil, nil
	}
	initial, exists := r.InitialUsageReservations[res.InitialUsageID]
	if !exists || initial.ExecutionEpoch != res.ExecutionEpoch || initial.ScopeKey != res.ScopeKey {
		return in, nil, invalidCorrection("observation has no original initial invocation")
	}

	if err := validateDisjointUsageComponents(r, in, res); err != nil {
		return in, nil, err
	}
	marks := make(map[string]float64, len(r.UsageSessionMarks)+len(in.Observation.Sessions))
	for k, v := range r.UsageSessionMarks {
		marks[k] = v
	}
	u := OutputCorrectionUsage{Tokens: in.Observation.Tokens, UnreportedCalls: in.Observation.UnreportedCalls, CostUSD: in.Observation.OtherCostUSD}
	for _, s := range in.Observation.Sessions {
		k := sessionCostKey(s)
		u.CostUSD += max(0, s.CostUSD-marks[k])
		marks[k] = max(marks[k], s.CostUSD)
	}
	if !validCorrectionUsage(u) {
		return in, nil, invalidCorrection("observed usage overflow")
	}
	if in.Observation.Version == 2 {
		projection, projected, err := projectUsage(*in.Observation, r.UsageSessionMarks)
		if err != nil {
			return in, nil, err
		}
		if projected != u {
			return in, nil, invalidCorrection("projection does not conserve settled usage")
		}
		in.Projection = projection
	}
	in.Usage = u
	return in, marks, nil
}
func InitialUsageTarget(r OutputCorrectionReservation) OutputCorrectionReceiptTarget {
	return OutputCorrectionReceiptTarget{Kind: "initial", LedgerKey: r.ID, Generation: r.ID, ReservationID: r.ID, ExecutionEpoch: r.ExecutionEpoch}
}
func ApplyInitialUsageReservation(r *Run, a RunExecutionAuthority, next OutputCorrectionReservation) (bool, error) {
	if !a.Matches(r) {
		return false, nil
	}
	var freezeErr error
	next, freezeErr = FreezeInitialUsageReservation(next)
	if freezeErr != nil {
		return false, freezeErr
	}
	if next.Origin != nil && (next.Origin.RunID != r.ID || next.Origin.TenantID != r.TenantID) {
		return false, invalidCorrection("usage origin run identity differs")
	}
	if !utf8.ValidString(next.LogicalInvocationID) || next.ID == "" || next.ExecutionEpoch != a.Epoch || next.ScopeKey == "" || next.StartedAt.IsZero() || next.Receipt != nil {
		return false, invalidCorrection("invalid initial reservation")
	}
	if next.InitialUsageID != "" && next.InitialUsageID != next.ID {
		return false, invalidCorrection("initial identity differs")
	}
	next.InitialUsageID = next.ID
	next.StartedAt = canonicalCorrectionTime(next.StartedAt)
	if old, ok := r.InitialUsageReservations[next.ID]; ok {
		if sameCorrectionReservation(old, next) {
			return false, nil
		}
		return false, invalidCorrection("contradictory initial reservation")
	}
	copy := make(map[string]OutputCorrectionReservation, len(r.InitialUsageReservations)+1)
	for k, v := range r.InitialUsageReservations {
		copy[k] = v
	}
	copy[next.ID] = next
	r.InitialUsageReservations = copy
	return true, nil
}
func applyInitialUsageReceipt(r *Run, target OutputCorrectionReceiptTarget, in OutputCorrectionReceipt) (bool, error) {
	if in.Observation == nil {
		return false, invalidCorrection("initial receipt requires accounting observation")
	}
	res, exists := r.InitialUsageReservations[target.ReservationID]
	if !exists || target.Kind != "initial" || target.LedgerKey != res.ID || target.Generation != res.ID || target.ExecutionEpoch != res.ExecutionEpoch {
		return false, invalidCorrection("initial receipt identity differs")
	}
	if res.Receipt != nil {
		if sameIncomingReceipt(*res.Receipt, in) {
			return false, nil
		}
		return false, invalidCorrection("contradictory initial receipt")
	}
	settled, marks, err := observedReceipt(r, in, res)
	if err != nil {
		return false, err
	}
	res.Receipt = &settled
	copy := make(map[string]OutputCorrectionReservation, len(r.InitialUsageReservations))
	for k, v := range r.InitialUsageReservations {
		copy[k] = v
	}
	copy[res.ID] = res
	r.InitialUsageReservations = copy
	if marks != nil {
		r.UsageSessionMarks = marks
	}
	return true, nil
}
func (s *FilesystemRunStore) ReserveInitialUsageIf(ctx context.Context, id string, a RunExecutionAuthority, next OutputCorrectionReservation) (bool, error) {
	var err error
	next, err = FreezeInitialUsageReservation(next)
	if err != nil {
		return false, err
	}
	r, unlock, err := s.lockExistingRunMetadata(ctx, id)
	if err != nil {
		return false, err
	}
	defer unlock()
	epoch := r.ExecutionEpoch
	ok, err := ApplyInitialUsageReservation(r, a, next)
	if !ok || err != nil {
		return ok, err
	}
	r.UpdatedAt = time.Now().UTC()
	if err = s.writeRunLockedContext(ctx, r, false, &epoch); err != nil {
		return false, err
	}
	return true, nil
}

// FreezeOutputCorrectionReceipt snapshots the request before waiting for a
// lock/network CAS. A pure retry must never read a later caller-owned slice.
func FreezeOutputCorrectionReceipt(in OutputCorrectionReceipt) (OutputCorrectionReceipt, error) {
	return normalizeObservationReceipt(in)
}

// ObservedExecutionStore is the complete accounting-capable execution protocol.
type ObservedExecutionStore interface {
	OutputCorrectionCASStore
	ReserveInitialUsageIf(context.Context, string, RunExecutionAuthority, OutputCorrectionReservation) (bool, error)
}

func AsObservedExecutionStore(s RunStore) ObservedExecutionStore {
	return capability[ObservedExecutionStore](s)
}
