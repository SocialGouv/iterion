package model

import (
	"cmp"
	"slices"

	"github.com/SocialGouv/iterion/pkg/store"
)

// OutputCorrectionSessionCost is a producer-declared session high-water, not
// output supplied by a model. Identity includes the serving backend and
// credential fingerprint. It is transient accounting input for the owning
// caller; it is not a durable episode or an authorization grant.
type OutputCorrectionSessionCost struct {
	Backend     string
	SessionID   string
	Fingerprint string
	CostUSD     float64
}

// OutputCorrectionAccounting separates cumulative session readings from
// additive usage. Tokens and unreported calls are always additive. Total
// includes the first answer and all rounds; CorrectOutputWithUsage returns
// only the last round's marginal usage. OtherCostUSD includes extraction and
// costs whose producer did not declare a comparable cumulative session.
type OutputCorrectionAccounting struct {
	Total        store.OutputCorrectionUsage
	Sessions     []OutputCorrectionSessionCost
	OtherCostUSD float64
}

func (h *OutputCorrectionHandle) Accounting() OutputCorrectionAccounting {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := OutputCorrectionAccounting{Total: store.OutputCorrectionUsage{Tokens: h.aggregate.Tokens, CostUSD: h.aggregate.CostUSD, UnreportedCalls: h.aggregate.UnreportedCalls}, OtherCostUSD: h.aggregate.CostUSD}
	for key, cost := range h.sessionCosts {
		if key.id == "" {
			continue
		}
		out.Sessions = append(out.Sessions, OutputCorrectionSessionCost{Backend: key.backend, SessionID: key.id, Fingerprint: key.fingerprint, CostUSD: cost})
		out.OtherCostUSD -= cost
	}
	// Floating-point summation may differ in the last bit from subtraction.
	out.OtherCostUSD = max(0, out.OtherCostUSD)
	slices.SortFunc(out.Sessions, func(a, b OutputCorrectionSessionCost) int {
		return cmp.Or(cmp.Compare(a.Backend, b.Backend), cmp.Compare(a.SessionID, b.SessionID), cmp.Compare(a.Fingerprint, b.Fingerprint))
	})
	return out
}
