package model

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The launch-time provider vocabulary is TWO lists, one derivation (ADR-131
// §5): each reader takes the list its question needs.
//
//   - KnownPoolProviders is what a credential POOL may lend a key for: the
//     vendor providers. The legacy pool, the fallback door and the pool wants
//     read it, so they never ask a donor for a gateway key — a gateway key
//     authenticates against its own tenant's endpoint (an attached pool
//     brings the gateway want in through the attachment alone, ADR-164).
//   - KnownRoutingProviders is what the WALK reads routes and tiers against:
//     every provider a credential tier can hold a key for, the pool's list
//     plus the openai_compatible gateway. The cloud publisher's credential
//     walk, its funding refusals, its allowlist gate and preview, and the
//     in-process lane's gate (#2665 item 7) read it.
//
// A vendor provider joins KnownPoolProviders and so reaches every reader the
// same day; only a provider the pool must never lend joins the walk alone —
// TestKnownPoolProvidersAreTheRoutingVocabularyLessTheGateway pins the
// difference to the gateway.
var KnownPoolProviders = []secrets.Provider{
	secrets.ProviderAnthropic,
	secrets.ProviderOpenAI,
	secrets.ProviderBedrock,
	secrets.ProviderVertex,
	secrets.ProviderAzure,
	secrets.ProviderOpenRouter,
	secrets.ProviderXAI,
	secrets.ProviderZAI,
	secrets.ProviderMoonshot,
}

// KnownRoutingProviders is the walk's vocabulary: KnownPoolProviders, in its
// order, then the gateway — last, so every reader that walks the list in
// order (the tier fills, the restore) meets the vendor providers where it
// always did.
var KnownRoutingProviders = append(append([]secrets.Provider(nil), KnownPoolProviders...), secrets.ProviderOpenAICompatible)

// KnownProviderSet is KnownRoutingProviders as the lower-cased set
// EffectiveProviders takes — the walk's vocabulary.
func KnownProviderSet() map[string]bool { return providerSet(KnownRoutingProviders) }

// KnownPoolProviderSet is KnownPoolProviders as the lower-cased set
// EffectiveProviders takes — the pool wants' vocabulary, under which a
// gateway route names nothing (RouteView.GatewayKeyFunded).
func KnownPoolProviderSet() map[string]bool { return providerSet(KnownPoolProviders) }

func providerSet(list []secrets.Provider) map[string]bool {
	m := make(map[string]bool, len(list))
	for _, p := range list {
		m[strings.ToLower(string(p))] = true
	}
	return m
}

// AllowlistRefusal is the gate half of ADR-129 §5 on a finished walk: a
// route the list cannot check refuses first (the deterministic head of the
// unreadable list), then a PRIMARY route with no allowed server left
// refuses naming node, provider and the level that set the list. nil = the
// launch may proceed (a pruned rescue is the caller's to log). ONE body for
// every lane that enforces the list — the cloud publisher before it seals,
// the in-process lane before it builds its executor — so the refusal an
// operator reads is the same whichever process launched.
func AllowlistRefusal(walk ProviderResolution, level string) error {
	if u := FirstUnreadable(walk); u != nil {
		return &RouteUnreadableError{Site: u.Site, Field: u.Field, Hint: u.Hint}
	}
	if site, prov, ok := FirstForbiddenPrimary(walk); ok {
		return &ProviderOutsideError{Site: site, Provider: prov, Level: level}
	}
	return nil
}
