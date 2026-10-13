package credpool

import (
	"testing"
	"time"
)

// The fallback door's consent filter (ADR-121 § Delivery 2, slice 5): a
// FallbackOnly consult serves only pledges whose donor marked
// `fallback_use` and NAMES the others; the whole-bundle consult never
// reads the mark.
// Mutants: drop the filter → the consenting-only case reds; filter
// unconditionally → the normal-path case reds; a generic skip status →
// the status case reds.
func TestEligiblePledges_fallbackFilter(t *testing.T) {
	mk := func(id string, fallbackUse bool) Pledge {
		return Pledge{ID: id, UserID: "donor-" + id,
			Credential: Credential{Source: SourceAPIKey, Ref: "zai"},
			Enabled:    true, Health: HealthOK, FallbackUse: fallbackUse}
	}
	want := Credential{Source: SourceAPIKey, Ref: "zai"}
	req := Request{UserID: "requester", FallbackOnly: true, Wants: []Credential{want}}
	now := time.Now()

	cands := []Pledge{mk("consenting", true), mk("silent", false)}
	eligible, skips := eligiblePledges(cands, req, want, now)
	if len(eligible) != 1 || eligible[0].ID != "consenting" {
		t.Fatalf("eligible = %+v, want only the consenting pledge — the door is consent-gated", eligible)
	}
	if len(skips) != 1 || skips[0].PledgeID != "silent" || skips[0].Status != StatusNoFallbackConsent {
		t.Fatalf("skips = %+v, want the silent pledge named no_fallback_consent", skips)
	}

	// The whole-bundle consult (FallbackOnly unset) ignores the mark: a
	// run with nothing of its own is served by ANY active pledge.
	req.FallbackOnly = false
	eligible, skips = eligiblePledges(cands, req, want, now)
	if len(eligible) != 2 || len(skips) != 0 {
		t.Fatalf("normal-path eligible = %+v skips = %+v, want BOTH pledges — the mark adds a lane, it removes none", eligible, skips)
	}
}
