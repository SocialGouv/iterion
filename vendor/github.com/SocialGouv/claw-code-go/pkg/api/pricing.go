package api

import (
	"strings"

	"github.com/SocialGouv/claw-code-go/internal/apikit"
)

// ModelPricing is the per-million-token cost for a model in USD.
// Zero on either field means "unknown" — the live source did not
// publish pricing for that model and the caller should treat zero as
// "skip cost emission" rather than reporting $0.
type ModelPricing struct {
	InputUSDPerMillion  float64
	OutputUSDPerMillion float64
	Source              string // "live" when sourced from the OpenRouter cache; "" if not found
}

// LookupModelPricing returns the per-million-token cost for the given
// model, sourced from claw's live registry cache (OpenRouter, refreshed
// async every 24h). Provider-prefixed and tenant-prefixed model specs
// are stripped down to the trailing canonical name so callers can pass
// either "anthropic/claude-sonnet-4-6" or "claude-sonnet-4-6".
//
// Returns ok=false when:
//   - the live cache has not yet been populated (cold start, no
//     network) and the model is not in the embed registry
//   - the cache exists but the model has no pricing field
//   - the live registry was disabled via CLAW_DISABLE_LIVE_REGISTRY=1
//
// Callers concerned with hard budget tracking should still pull
// authoritative rates from their provider's invoices — this is a
// best-effort observability hint that follows OpenRouter's posted
// pricing.
func LookupModelPricing(model string) (ModelPricing, bool) {
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	if model == "" {
		return ModelPricing{}, false
	}

	// MaybeRefreshLive is async and best-effort; the call returns
	// immediately, populating the cache in a background goroutine if
	// stale. We read whatever is currently on disk. nil reg is fine —
	// mergeLiveIntoRegistry no-ops on it, so the side effect we want
	// (cache populated on disk) still happens.
	apikit.MaybeRefreshLive(nil)

	cache, err := apikit.LoadLiveCache()
	if err != nil || cache == nil {
		return ModelPricing{}, false
	}
	for _, entry := range cache.Entries {
		if entry.Canonical == model || containsAlias(entry.Aliases, model) {
			if entry.InputUSDPerM == 0 && entry.OutputUSDPerM == 0 {
				return ModelPricing{}, false
			}
			return ModelPricing{
				InputUSDPerMillion:  entry.InputUSDPerM,
				OutputUSDPerMillion: entry.OutputUSDPerM,
				Source:              "live",
			}, true
		}
	}
	return ModelPricing{}, false
}

func containsAlias(aliases []string, model string) bool {
	for _, a := range aliases {
		if a == model {
			return true
		}
	}
	return false
}
