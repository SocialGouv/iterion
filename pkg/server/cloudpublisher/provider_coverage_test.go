package cloudpublisher

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// A provider the publisher never tries to resolve cannot be spent: the key
// sits in the store, the bundle omits it, and the node dies "no credential"
// while its funding is one row away. Every provider iterion can hold a BYOK
// key for must be on this walk.
func TestAllKnownProvidersCoversEveryValidProvider(t *testing.T) {
	walked := map[secrets.Provider]bool{}
	for _, p := range allKnownProviders {
		walked[p] = true
		if !p.Valid() {
			t.Errorf("allKnownProviders holds %q, which Provider.Valid() refuses — the walk would resolve a provider no route can name", p)
		}
	}
	for _, p := range []secrets.Provider{
		secrets.ProviderAnthropic, secrets.ProviderOpenAI, secrets.ProviderBedrock,
		secrets.ProviderVertex, secrets.ProviderAzure, secrets.ProviderOpenRouter,
		secrets.ProviderXAI, secrets.ProviderZAI, secrets.ProviderMoonshot,
	} {
		if !walked[p] {
			t.Errorf("provider %q is valid but absent from allKnownProviders — a BYOK key for it would never reach a run", p)
		}
	}
}

// The meter backend decides whether a refused key is SKIPPED for the next
// run or handed out again. Both facades ride claude_code sessions, so both
// carry metered evidence; a provider missing here is never skipped, so the
// walk keeps leasing a key the fleet has already watched get walled.
func TestUsageBackendForProvider_CoversTheAnthropicWire(t *testing.T) {
	for _, p := range []secrets.Provider{secrets.ProviderAnthropic, secrets.ProviderZAI, secrets.ProviderMoonshot} {
		if got := usageBackendForProvider(p); got != delegate.BackendClaudeCode {
			t.Errorf("usageBackendForProvider(%q) = %q, want %q", p, got, delegate.BackendClaudeCode)
		}
	}
	if got := usageBackendForProvider(secrets.ProviderOpenAI); got != "" {
		t.Errorf("usageBackendForProvider(openai) = %q, want \"\"", got)
	}
}
