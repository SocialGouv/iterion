package cloudpublisher

import (
	"context"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// The ORG credential tier: an organization's own provider keys and forfait
// blobs, stored under a reserved scope (secrets.OrgTierTenantID /
// OrgTierOwnerKey) and lent to the teams its CredentialAudience admits.
//
// It exists because "share a key across the org's product teams" had no
// home: the API-key walk only sees the team's and the user's rows, and
// secrets.OrgOwnerKey — despite its name — keys a TEAM forfait. The only
// way to share was to copy the credential into every team, which is N
// writes per rotation and N places to forget one.
//
// Two properties make it safe, and both are the platform tier's:
//
//   - It fills per WIRE FAMILY, so an org key never lands next to a
//     credential the team already holds in another shape (the delegates
//     rank a ctx API key above a ctx OAuth dir on the same wire, so the
//     org key would silently serve every call).
//   - It is best-effort. A degraded store read leaves the slot to the tier
//     below rather than failing a launch the pool or the platform could
//     still serve.
//
// One inherited limitation to know: the mutualised pool is consulted only
// when a run holds NO credential at all, so an org key on a wire the
// workflow never uses (an OpenAI key under an Anthropic-only bot) makes the
// bundle non-empty and suppresses a pool donation that WOULD have served.
// That is the pool's own admission rule and predates this tier — a team or
// user key does exactly the same — so making eligibility route-aware is a
// change to the pool's contract, not to this one.
//
// One property is its own: the audience. The zero value admits nobody, so
// an org that has never lent anything behaves exactly as before.

// orgCredentialAudience answers whether teamID may spend orgID's shared
// credentials, and returns the org id to meter them under.
//
// FAIL CLOSED on a store error: an org tier that admitted a team because
// Mongo blinked would spend an org's subscription on a team its admins
// never named. Silence is the safe answer here precisely because a tier
// below (pool, platform, the runner's env) can still fund the run — the
// cost of a false negative is a fallback, the cost of a false positive is
// someone else's money.
func (p *Publisher) orgCredentialAudience(ctx context.Context, orgID, teamID string) bool {
	if orgID == "" || teamID == "" || p.identity == nil {
		return false
	}
	o, err := p.identity.GetOrg(ctx, orgID)
	if err != nil {
		if p.logger != nil {
			p.logger.Warn("cloudpublisher: org credential audience for org %s unreadable (%v) — the org tier is skipped for team %s", orgID, err, teamID)
		}
		return false
	}
	return o.CredentialAudience.Allows(teamID)
}

// fillFromOrg fills the API-key and OAuth slots still empty after the
// tenant's own tiers with the org's shared credentials, when the org's
// audience admits this team. Filled slots are recorded on
// bundle.OrgSourced so the runner meters them on the ORG's key rather than
// fragmenting one subscription across every team that borrowed it.
func (p *Publisher) fillFromOrg(
	ctx context.Context,
	runID, orgID, tenantID, botID string,
	bundle *secrets.RunBundle,
	apiKeyFPs map[secrets.Provider]string,
	skips *skipTracker,
	skippedAPIKeys map[secrets.Provider]skippedAPIKey,
	skippedForfaits map[string]skippedForfait,
) {
	if p.sealer == nil || !p.orgCredentialAudience(ctx, orgID, tenantID) {
		return
	}
	if bundle.OrgSourced == nil {
		bundle.OrgSourced = map[string]bool{}
	}

	taken := map[string]bool{}
	for prov := range bundle.APIKeys {
		taken[secrets.WireFamily(string(prov))] = true
	}
	for kind := range bundle.OAuthCredentials {
		taken[secrets.WireFamily(kind)] = true
	}
	fillable := func(slot string) bool { return !taken[secrets.WireFamily(slot)] }

	orgScope := secrets.OrgTierTenantID(orgID)
	meter := usagecap.OrgScope(orgID)

	// Org API keys live under the reserved scope; the ctx tenant must match
	// or the store's isolation filter (correctly) returns nothing.
	if p.apiKeys != nil {
		missing := make([]secrets.Provider, 0, len(allKnownProviders))
		for _, prov := range allKnownProviders {
			if fillable(string(prov)) {
				missing = append(missing, prov)
			}
		}
		if len(missing) > 0 {
			octx := store.WithTenant(ctx, orgScope)
			resolved, err := secrets.Resolve(octx, p.apiKeys, orgScope, "", missing, nil, p.sealer,
				p.apiKeyUsable(octx, meter, runID, botID, skips))
			if err != nil {
				p.logger.Warn("cloudpublisher: org api-key resolve for org %s: %v", orgID, err)
			} else {
				usedIDs := make([]string, 0, len(resolved))
				// Iterate `missing` (allKnownProviders order), NOT the
				// resolved map: when the org holds two keys on one wire
				// family (anthropic + zai both map to "anthropic-wire"),
				// fillable() lets only the first through — and map
				// iteration order is randomised, so which key funds a run
				// would flip between launches. The provider slice fixes
				// the winner.
				for _, prov := range missing {
					r, ok := resolved[prov]
					if !ok || len(r.Plaintext) == 0 || !fillable(string(prov)) {
						continue
					}
					bundle.APIKeys[prov] = string(r.Plaintext)
					bundle.OrgSourced[string(prov)] = true
					apiKeyFPs[prov] = r.Fingerprint
					taken[secrets.WireFamily(string(prov))] = true
					usedIDs = append(usedIDs, r.KeyID)
					p.logger.Info("cloudpublisher: org credential used run=%s org=%s slot=%s fp=%s", runID, orgID, prov, r.Fingerprint)
				}
				// A provider whose every org key was refused resolves to
				// nothing under the predicate. Remember what an unfiltered
				// walk would have chosen: if the end of the resolution finds
				// that wire still empty — no pool grant, no platform key —
				// the refused one is restored, because a run that makes one
				// refused call parks on a durable usage-window retry while a
				// run published with an empty wire fails on an auth error
				// nothing retries. The platform tier states the same rule;
				// omitting it here is what turned a recoverable park into an
				// outright refusal for an org-funded team.
				if refused := providersWithoutKey(missing, bundle.APIKeys); len(refused) > 0 {
					fallback, ferr := secrets.Resolve(octx, p.apiKeys, orgScope, "", refused, nil, p.sealer, nil)
					if ferr != nil {
						p.logger.Warn("cloudpublisher: org refused-key fallback resolve: %v", ferr)
					}
					for prov, r := range fallback {
						if len(r.Plaintext) == 0 {
							continue
						}
						if _, seen := skippedAPIKeys[prov]; seen {
							continue // a tenant key's restore takes precedence
						}
						skippedAPIKeys[prov] = skippedAPIKey{
							plaintext: string(r.Plaintext), keyID: r.KeyID,
							fingerprint: r.Fingerprint, org: true,
						}
					}
				}
				if len(usedIDs) > 0 {
					ids, t := usedIDs, time.Now().UTC()
					p.goSafeDetached("org-apikey-markused", func() {
						bg, cancel := context.WithTimeout(store.WithTenant(context.Background(), orgScope), 5*time.Second)
						defer cancel()
						for _, id := range ids {
							_ = p.apiKeys.MarkUsed(bg, id, t)
						}
					})
				}
			}
		}
	}

	// Org forfait blobs under the reserved owner key. Skipped — like the
	// api-key read above via its `missing` guard — when no OAuth kind is
	// still fillable, so a run already funded on both wires costs zero
	// extra store reads here.
	if p.oauthForfait == nil ||
		(!fillable(string(secrets.OAuthKindClaudeCode)) && !fillable(string(secrets.OAuthKindCodex))) {
		return
	}
	records, err := p.oauthForfait.ListByUser(ctx, secrets.OrgTierOwnerKey(orgID))
	if err != nil {
		p.logger.Warn("cloudpublisher: org oauth list for org %s: %v", orgID, err)
		return
	}
	for _, rec := range records {
		if !fillable(string(rec.Kind)) {
			continue
		}
		payload, err := secrets.OpenOAuthPayload(p.sealer, rec.UserID, rec.Kind, rec.SealedPayload)
		if err != nil {
			p.logger.Warn("cloudpublisher: unseal org oauth %s/%s: %v", rec.UserID, rec.Kind, err)
			continue
		}
		// Unlike the platform tier, the org tier is NOT the last DB-backed
		// stop: the pool and the platform sit below it. So a forfait whose
		// provider window is closed is passed over rather than handed to
		// the run — the fallback chain can still serve it immediately,
		// which beats parking until a weekly window resets.
		// `meter`, not the store scope: the runner records this credential's
		// readings under usagecap.OrgScope(orgID), so reading any other key
		// asks a ledger nobody writes.
		if until, why := p.forfaitWindowClosed(ctx, meter, secrets.OrgTierOwnerKey(orgID), botID, rec, payload); !until.IsZero() {
			p.logger.Info("cloudpublisher: oauth-forfait(org) SKIPPED for run=%s org=%s kind=%s fp=%s — %s (reopens %s); falling through to the next credential tier",
				runID, orgID, rec.Kind, rec.Fingerprint, why, until.UTC().Format(time.RFC3339))
			skips.note(until)
			if _, seen := skippedForfaits[string(rec.Kind)]; !seen {
				skippedForfaits[string(rec.Kind)] = skippedForfait{payload: payload, fp: rec.Fingerprint, org: true}
			}
			continue
		}
		bundle.OAuthCredentials[string(rec.Kind)] = payload
		bundle.OrgSourced[string(rec.Kind)] = true
		setOAuthFingerprint(bundle, string(rec.Kind), rec.Fingerprint)
		taken[secrets.WireFamily(string(rec.Kind))] = true
		p.logger.Info("cloudpublisher: org credential used run=%s org=%s slot=%s fp=%s", runID, orgID, rec.Kind, rec.Fingerprint)
	}
}

// OrgCredentialAudienceOf is the read the server's org-credential routes
// use to render "who may spend this". Kept beside the resolver so the API
// and the publisher can never disagree about the predicate.
func OrgCredentialAudienceOf(o identity.Org) identity.CredentialAudience { return o.CredentialAudience }

// platformAudienceAllows reports whether this run may draw on the PLATFORM
// tier — the deployment's own credentials, which every tenant without one
// of its own used to reach in silence.
//
// It admits by default, and that direction is the opposite of the org
// tier's on purpose. The org tier lends a key someone chose to lend, so an
// unreadable policy must not lend it. The platform tier is what the
// deployment already runs on, so an unreadable policy must not take it
// away: failing closed there would turn a settings blip into a fleet-wide
// outage, which is exactly the failure mode the probes doc warns about for
// critical checks on shared backends.
//
// The refusal, when it does happen, is LOGGED with the tenant named: a run
// that silently receives no credential fails at its first LLM call with a
// provider error, and nothing downstream would say the audience was why.
func (p *Publisher) platformAudienceAllows(ctx context.Context, runID, orgID, tenantID string) bool {
	if p.platformAudience == nil {
		return true
	}
	rec := p.platformAudience.Get(ctx)
	if rec.Allows(orgID, tenantID) {
		return true
	}
	if p.logger != nil {
		p.logger.Warn("cloudpublisher: platform credential tier REFUSED for run=%s tenant=%s org=%s — the platform credential audience does not admit it; the run must bring its own credential or be added to the audience",
			runID, tenantID, orgID)
	}
	return false
}
