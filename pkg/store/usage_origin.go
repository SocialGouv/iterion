package store

import (
	"context"
	"reflect"
	"sort"
	"time"
	"unicode/utf8"
)

// UsageOrigin is immutable provenance for one queue publication. It conveys no
// execution authority. Local and older invocations have no origin.
type UsageOrigin struct {
	Version       int    `json:"version" bson:"version"`
	RunID         string `json:"run_id" bson:"run_id"`
	Publication   string `json:"publication" bson:"publication"`
	TenantID      string `json:"tenant_id" bson:"tenant_id"`
	OrgID         string `json:"org_id,omitempty" bson:"org_id,omitempty"`
	PoolGrantless bool   `json:"pool_grantless,omitempty" bson:"pool_grantless,omitempty"`
}

func CanonicalUsageOrigin(in UsageOrigin) (UsageOrigin, error) {
	if in.Version != 1 || in.RunID == "" || in.TenantID == "" {
		return in, invalidCorrection("invalid usage origin")
	}
	for _, s := range []string{in.RunID, in.Publication, in.TenantID, in.OrgID} {
		if !utf8.ValidString(s) {
			return in, invalidCorrection("usage origin is not UTF-8")
		}
	}
	stamp, err := time.Parse(time.RFC3339Nano, in.Publication)
	if err != nil || stamp.IsZero() {
		return in, invalidCorrection("invalid original publication")
	}
	in.Publication = stamp.UTC().Format(time.RFC3339Nano)
	return in, nil
}
func sameUsageOriginIdentity(a, b UsageOrigin) bool {
	return a.Version == b.Version && a.RunID == b.RunID && a.Publication == b.Publication
}

// FreezeInitialUsageReservation copies mutable provenance before a store waits
// on its lock or enters a retried CAS. Receipt is forbidden at reservation time.
func FreezeInitialUsageReservation(in OutputCorrectionReservation) (OutputCorrectionReservation, error) {
	if in.Receipt != nil {
		return in, invalidCorrection("initial reservation already has receipt")
	}
	if in.Origin != nil {
		origin, err := CanonicalUsageOrigin(*in.Origin)
		if err != nil {
			return in, err
		}
		in.Origin = &origin
	}
	return in, nil
}

type SettledUsageReceipt struct {
	Projection  *UsageProjection
	Origin      UsageOrigin
	Target      OutputCorrectionReceiptTarget
	Usage       OutputCorrectionUsage
	Observation UsageObservation
}

// ReceiptsForOrigin projects settled facts, including retired generations. It
// deliberately ignores the current status/epoch/publication and creates no
// execution grant or financial posting.
func ReceiptsForOrigin(r *Run, origin UsageOrigin) ([]SettledUsageReceipt, error) {
	origin, err := CanonicalUsageOrigin(origin)
	if err != nil {
		return nil, err
	}
	if r == nil || r.ID != origin.RunID || r.TenantID != origin.TenantID {
		return nil, invalidCorrection("usage origin run identity differs")
	}
	initial := map[string]OutputCorrectionReservation{}
	for id, res := range r.InitialUsageReservations {
		if res.Origin == nil {
			continue
		}
		own, err := CanonicalUsageOrigin(*res.Origin)
		if err != nil {
			return nil, err
		}
		if own != *res.Origin {
			return nil, invalidCorrection("noncanonical persisted usage origin")
		}
		if !sameUsageOriginIdentity(own, origin) {
			continue
		}
		if own != origin {
			return nil, invalidCorrection("contradictory usage origin body")
		}
		if id == "" || res.ID != id || res.InitialUsageID != id || res.ExecutionEpoch == "" || res.ScopeKey == "" {
			return nil, invalidCorrection("invalid initial origin identity")
		}
		initial[id] = res
	}
	seen := map[OutputCorrectionReceiptTarget]SettledUsageReceipt{}
	add := func(target OutputCorrectionReceiptTarget, res OutputCorrectionReservation) error {
		if res.Receipt == nil {
			return nil
		}
		frozen, err := FreezeSettledUsageReceipt(*res.Receipt)
		if err != nil {
			return err
		}
		item := SettledUsageReceipt{Origin: origin, Target: target, Usage: frozen.Usage, Observation: *frozen.Observation, Projection: frozen.Projection}
		if old, exists := seen[target]; exists && !reflect.DeepEqual(old, item) {
			return invalidCorrection("contradictory duplicate receipt target")
		}
		seen[target] = item
		return nil
	}
	for _, res := range initial {
		if err := add(InitialUsageTarget(res), res); err != nil {
			return nil, err
		}
	}
	episode := func(key string, ep OutputCorrectionEpisode) error {
		for _, res := range ep.Reservations {
			first, ok := initial[res.InitialUsageID]
			if !ok {
				continue
			}
			if key == "" || ep.LedgerKey != key || ep.Generation == "" || res.ID == "" || res.Origin != nil || res.ExecutionEpoch != first.ExecutionEpoch || res.ScopeKey != first.ScopeKey {
				return invalidCorrection("round origin identity differs")
			}
			if first.Receipt == nil {
				return invalidCorrection("round lacks settled origin initial")
			}
			if err := add(res.ReceiptTarget(key, ep.Generation), res); err != nil {
				return err
			}
		}
		return nil
	}
	for key, ep := range r.OutputCorrections {
		if err := episode(key, ep); err != nil {
			return nil, err
		}
	}
	for _, ep := range r.OutputCorrectionHistory {
		if err := episode(ep.LedgerKey, ep); err != nil {
			return nil, err
		}
	}
	out := make([]SettledUsageReceipt, 0, len(seen))
	for _, v := range seen {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Target, out[j].Target
		for _, p := range [][2]string{{a.Kind, b.Kind}, {a.LedgerKey, b.LedgerKey}, {a.Generation, b.Generation}, {a.ReservationID, b.ReservationID}, {a.ExecutionEpoch, b.ExecutionEpoch}} {
			if p[0] != p[1] {
				return p[0] < p[1]
			}
		}
		return false
	})
	return out, nil
}
func ReadReceiptsForOrigin(ctx context.Context, runs RunStore, origin UsageOrigin) ([]SettledUsageReceipt, error) {
	origin, err := CanonicalUsageOrigin(origin)
	if err != nil {
		return nil, err
	}
	r, err := runs.LoadRun(ctx, origin.RunID)
	if err != nil {
		return nil, err
	}
	return ReceiptsForOrigin(r, origin)
}
