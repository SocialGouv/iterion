package model

import (
	"strings"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// KnownRoutingProviders is the provider vocabulary every launch-time walk
// reads routes against — the providers a credential tier can hold a key
// for. ONE list: the cloud publisher's pool tiers and preview and the
// in-process lane's gate (#2665 item 7) all read it, so a provider joining
// the vocabulary reaches every reader the same day.
var KnownRoutingProviders = []secrets.Provider{
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

// KnownProviderSet is KnownRoutingProviders as the lower-cased set
// EffectiveProviders takes.
func KnownProviderSet() map[string]bool {
	m := make(map[string]bool, len(KnownRoutingProviders))
	for _, p := range KnownRoutingProviders {
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
