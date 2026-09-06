package runner

import (
	"context"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/credusage"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The per-CREDENTIAL half of an attempt's metering (#641), beside
// recordOrgSpend's per-ORG half.
//
// The org bucket charges RunTotals() to the org whatever tier served the
// run, so it answers "how much did this org consume" and never "what did
// this key cost". Two things make the second question answerable, and both
// are load-bearing:
//
//   - the spend is taken per (backend, model) ROUTE, not as a run total: a
//     run can spend a claude_code forfait on its implementer and a platform
//     codex key on its plan review, and one number belongs to neither;
//   - what a route cannot be attributed to is charged to NOBODY. A route
//     whose provider the run holds no credential for would otherwise land on
//     whichever fingerprint a default precedence picked, which is the
//     misattribution the counter exists to remove.

// recordCredentialSpend charges each of the attempt's routes to the
// credential that served it. Best effort throughout: a missing counter, an
// unattributable route or a store failure leave the observation on the
// floor rather than fail a finished run. Detached context (5s) for the same
// reason recordOrgSpend detaches — a cancelled run still spent.
func (r *Runner) recordCredentialSpend(ctx context.Context, msg *queue.RunMessage, usage *metricsEmitter, at time.Time) {
	if r.cfg.CredUsage == nil || usage == nil {
		return
	}
	creds, ok := secrets.CredentialsFromContext(ctx)
	if !ok {
		return
	}
	routes := usage.RouteTotals()
	if len(routes) == 0 {
		return
	}
	bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for route, totals := range routes {
		slot := credentialSlotForRoute(creds, route.backend, route.model)
		if slot == "" {
			// Declined for good: the attempt is over and nothing will name
			// this route's credential later, so the decline is a warning —
			// a debug line is silent on a production runner. Once per route.
			if r.cfg.Logger != nil && usage.noteDeclinedRoute(route) {
				r.cfg.Logger.Warn("runner: run %s spent %d tokens ($%.4f) on %s/%s with no credential iterion can name (wire %q) — not metered per credential",
					msg.RunID, totals.inputTokens+totals.outputTokens, totals.costUSD,
					route.backend, route.model, wireForRoute(route.backend, route.model))
			}
			continue
		}
		fp := creds.Fingerprint(slot)
		if fp == "" {
			// A slot with no fingerprint names a SLOT, not an account:
			// counting it would merge every unstamped credential of that
			// provider into one bucket.
			continue
		}
		spend := credusage.Spend{
			Key: credusage.Key{
				Fingerprint: fp,
				Provider:    slot,
				Tier:        credentialTier(creds, slot),
				TenantID:    msg.TenantID,
			},
			Nature:       credentialNature(slot),
			Backend:      route.backend,
			CostUSD:      totals.costUSD,
			InputTokens:  totals.inputTokens,
			OutputTokens: totals.outputTokens,
		}
		if err := r.cfg.CredUsage.AddSpend(bg, at, spend); err != nil && r.cfg.Logger != nil {
			r.cfg.Logger.Warn("runner: credential spend record for %s (run %s): %v", fp, msg.RunID, err)
		}
	}
}

// credentialNature says what a credential's dollar figure MEANS.
//
// An OAuth slot is a subscription: the provider bills nothing per call, and
// the figure a CLI prints is what those calls WOULD have cost metered. An
// API-key slot is real money on someone's invoice. Same line
// credpool.CredentialSource.Metered() draws for a lent credential, applied
// to the slot shape here so pkg/credusage stays a leaf.
func credentialNature(slot string) credusage.Nature {
	if secrets.OAuthKind(slot).Valid() {
		return credusage.NatureEstimate
	}
	return credusage.NatureMetered
}

// credentialTier names which resolution tier supplied the slot. The tier is
// part of the meter's identity, not a label: a key lent through the pool and
// the same key used by its owner are two different economic facts.
func credentialTier(creds secrets.Credentials, slot string) credusage.Tier {
	switch {
	case creds.IsPoolSourced(slot):
		return credusage.TierPool
	case creds.IsPlatformSourced(slot):
		return credusage.TierPlatform
	default:
		return credusage.TierTeam
	}
}

// credentialSlotForRoute names the credential slot a (backend, model) route
// spent, or "" when the run holds none for it.
//
// The MODEL names the provider where it carries a prefix, because a route is
// the pair: a claw node can be pointed at any provider the registry knows,
// so the backend alone does not say what was spent. Without a prefix the
// backend's own wire decides — claude_code and claw default to the anthropic
// wire, codex to the openai one — and a backend that resolves its
// credentials from its own config (kimi, grok) names nothing, because
// iterion did not supply what they spent.
//
// Within a wire the slot follows the delegates' own precedence
// (anthropicCredEnvForCLI's contract): the z.ai facade token, then an
// Anthropic API key, then the OAuth forfait; on the openai wire, the API key
// then the codex forfait. Reading it differently here would credit a
// credential the delegate did not use.
func credentialSlotForRoute(creds secrets.Credentials, backend, modelName string) string {
	wire := wireForRoute(backend, modelName)
	switch wire {
	case anthropicWire:
		return firstHeldSlot(creds,
			string(secrets.ProviderZAI),
			string(secrets.ProviderAnthropic),
			delegate.BackendClaudeCode,
		)
	case openaiWire:
		return firstHeldSlot(creds,
			string(secrets.ProviderOpenAI),
			string(secrets.OAuthKindCodex),
		)
	case "":
		return ""
	default:
		// A single-shape provider (xai, openrouter, …): the slot IS the
		// provider, held or not.
		return firstHeldSlot(creds, wire)
	}
}

const (
	anthropicWire = "anthropic"
	openaiWire    = "openai"
)

// wireForRoute resolves a route to the credential wire it draws on, or ""
// when iterion cannot say.
func wireForRoute(backend, modelName string) string {
	if prov := providerFromModel(modelName); prov != "" {
		return prov
	}
	switch backend {
	// claw and claude_code both default to the anthropic wire when the
	// model carries no provider prefix.
	case delegate.BackendClaudeCode, "claw":
		return anthropicWire
	case delegate.BackendCodex:
		return openaiWire
	}
	// pi, kimi, grok and anything new: a bare model id says nothing, and
	// kimi/grok resolve their own credentials anyway.
	return ""
}

// providerFromModel reads the `provider/model` prefix iterion's model specs
// carry, normalised to a secrets.Provider name. "" for a bare id or an
// unknown provider — an unknown one must charge nobody rather than fall onto
// a default.
func providerFromModel(modelName string) string {
	idx := strings.Index(modelName, "/")
	if idx <= 0 {
		return ""
	}
	prov := secrets.Provider(strings.ToLower(strings.TrimSpace(modelName[:idx])))
	if !prov.Valid() {
		return ""
	}
	return string(prov)
}

// firstHeldSlot returns the first named slot the run actually carries a
// credential for.
func firstHeldSlot(creds secrets.Credentials, slots ...string) string {
	for _, slot := range slots {
		if secrets.OAuthKind(slot).Valid() {
			if creds.OAuthDir(slot) != "" {
				return slot
			}
			continue
		}
		if creds.APIKey(secrets.Provider(slot)) != "" {
			return slot
		}
	}
	return ""
}
