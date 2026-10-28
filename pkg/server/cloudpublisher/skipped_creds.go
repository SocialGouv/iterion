package cloudpublisher

import (
	"sort"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The walk's memory of what it passed over, read by the restore step at the
// end of resolveAndSealCredentials.
//
// EVERY candidate is remembered, per slot (a provider for an api key, a kind
// for a forfait) and per TIER, because the restore's refusals are not all
// about the slot: a credential a RESERVATION holds is refused for being
// spoken for, and the sibling it masked is the one the restore exists to
// recover. Keeping one entry per slot — one per provider for keys, one per
// kind for forfaits, with the shared tiers declining to record their own
// when the slot was already "seen" — meant that when the remembered entry
// was the reserved one, the restore refused it (rightly: handing it over is
// the starvation the reserve exists to prevent) and the DEAD sibling had
// never been recorded. Nothing came back, and the run went out with an empty
// wire to die on a no-credential auth error nothing retries, instead of
// parking on a durable usage-window retry.
//
// The asymmetry was the whole bug: skipTracker.floorHeld is keyed by
// FINGERPRINT precisely so a dead credential and a reserved one of the same
// provider are told apart, and the candidate memory never got the same
// treatment.

// skippedAPIKeys remembers every refused-but-held-back key of a provider, in
// the order the fills preferred them, tagged with the tier each came from
// (skippedAPIKey.org / .platform, read through restoreTierOf).
type skippedAPIKeys map[secrets.Provider][]skippedAPIKey

// remember records one candidate unless its FINGERPRINT is already known:
// the fingerprint is the credential's identity, so a record two tiers can
// both see is offered once. A candidate with no fingerprint is still
// remembered — it cannot be floor-held (only a metered credential can be),
// so it is restorable by definition, and dropping it would lose a park.
func (m skippedAPIKeys) remember(prov secrets.Provider, sk skippedAPIKey) {
	if sk.fingerprint != "" {
		for _, known := range m[prov] {
			if known.fingerprint == sk.fingerprint {
				return
			}
		}
	}
	m[prov] = append(m[prov], sk)
}

// candidatesAt returns the provider's candidates that came from one tier, in
// the order the fill preferred them. The restore walks the tiers in their own
// order and asks per tier, so the answer never depends on the order the
// candidates ARRIVED — two tiers filling in either sequence restore the same
// credential.
func (m skippedAPIKeys) candidatesAt(prov secrets.Provider, tier restoreTier) []skippedAPIKey {
	var out []skippedAPIKey
	for _, sk := range m[prov] {
		if restoreTierOf(sk.org, sk.platform) == tier {
			out = append(out, sk)
		}
	}
	return out
}

// skippedForfaits is the forfait twin: every window-closed subscription of a
// kind, in fill order, tagged with its tier. One tenant tier can hold two of
// one kind (a member's own subscription and the team's), so the slice is not
// a formality here either.
type skippedForfaits map[string][]skippedForfait

// remember records one candidate unless its fingerprint is already known.
func (m skippedForfaits) remember(kind string, sf skippedForfait) {
	if sf.fp != "" {
		for _, known := range m[kind] {
			if known.fp == sf.fp {
				return
			}
		}
	}
	m[kind] = append(m[kind], sf)
}

// kinds returns the remembered kinds in a STABLE order. Ranging over the map
// would make which kind the restore reaches first a property of Go's map
// seed, and "the first one wins" is only a fact when the order is one.
func (m skippedForfaits) kinds() []string {
	out := make([]string, 0, len(m))
	for kind := range m {
		out = append(out, kind)
	}
	sort.Strings(out)
	return out
}

// candidatesAt returns the kind's candidates from one tier, in fill order.
func (m skippedForfaits) candidatesAt(kind string, tier restoreTier) []skippedForfait {
	var out []skippedForfait
	for _, sf := range m[kind] {
		if restoreTierOf(sf.org, sf.platform) == tier {
			out = append(out, sf)
		}
	}
	return out
}

// deferredTenantKey is one tenant-tier candidate the restore set aside for
// the family's last park point: the provider AND the candidate, together,
// because the provider alone would send the tail back to the memory to pick
// "the first entry", which by then may be another tier's.
type deferredTenantKey struct {
	prov secrets.Provider
	sk   skippedAPIKey
}

// deferredPreviewKey is deferredTenantKey's twin in the credential preview:
// the provider and the CANDIDATE index the tenant pass set aside, so the
// preview's last-park-point tail names the same credential the live one does.
type deferredPreviewKey struct {
	prov secrets.Provider
	i    int
}

// restorableKey is the predicate the refused-key FALLBACK lookup walks with
// (the second, unfiltered resolve each tier runs to learn what it would have
// chosen had nothing been refused).
//
// It asks what that walk would have chosen among the candidates the restore
// may actually hand back, and a credential a reservation holds is not one of
// them: selecting it would record the reserved key as the provider's
// candidate and mask the sibling behind it, which is the masking this memory
// exists to end. Every OTHER reason the restore refuses a candidate is a
// property of the slot, not of the credential — the routing whitelist, a wire
// family another credential took, the refused_pinned_key knob — identical for
// every key of the provider, so no sibling would do better and the lookup
// does not model them.
//
// A PINNED key bypasses this predicate inside secrets.Resolve, as it bypasses
// the usability one: the operator named that key. A pin that opens fills its
// provider's slot, so it never reaches this lookup at all.
// A credential this lookup passes over therefore leaves no candidate behind,
// and the restore says nothing about it: the trace lives where the walk
// decided, beside the skip (apiKeyUsable logs the reserve by name), and the
// credential preview still carries it to the operator — it records every
// candidate, held ones included, and names the reserve on them.
func restorableKey(skips *skipTracker) func(secrets.ApiKey) bool {
	return func(k secrets.ApiKey) bool {
		return !skips.heldByFloor(k.Fingerprint)
	}
}
