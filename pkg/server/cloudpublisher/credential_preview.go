package cloudpublisher

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// PreviewCredentials interprets the live tier plan against metadata. It never
// calls Resolve, Acquire, a sealer, a provider probe, or a persistence method.
// The selected slots assume materialization succeeds; unknowns stay explicit.
func (p *Publisher) PreviewCredentials(ctx context.Context, spec runview.CredentialPreviewSpec, wf *ir.Workflow) (runview.CredentialPreview, error) {
	if p.runSecrets == nil || p.sealer == nil || spec.Context.TeamID == "" {
		return runview.CredentialPreview{}, runview.ErrCredentialPreviewUnavailable
	}
	x := &credentialPreview{
		p: p, ctx: store.WithTenant(ctx, spec.Context.TeamID),
		out: runview.CredentialPreview{ObservedAt: time.Now().UTC(), Context: spec.Context,
			Candidates: []runview.CredentialPreviewCandidate{}, Wires: []runview.CredentialPreviewWire{},
			Pool:     runview.CredentialPreviewPool{Reason: "credentials_present", Wants: []string{}},
			Warnings: []string{"Observation only: no secret was opened, no provider was probed, and no capacity was reserved. Selected slots assume successful materialization and admission; model routing decides which slot is spent. Alternatives show current metadata, not a guaranteed future grant.", "Runner environment credentials and required workflow secrets are outside this preview."}},
		api: map[secrets.Provider]int{}, pinnedAPI: map[secrets.Provider]int{}, oauth: map[string]int{}, skippedAPI: map[secrets.Provider]int{}, skippedOAuth: map[string]int{}, accountGroups: map[string]string{},
	}
	x.orgID = p.orgIDForTeam(ctx, spec.Context.TeamID)
	// The same canonicalisation the live walk applies, or the preview would
	// answer for a spelling the launch never uses.
	previewBotID := botregistry.NormalizeName(spec.Context.BotID)
	// The same derivation the launch stamps on the run document. Derived
	// fresh here on purpose: a preview describes a launch that has not
	// happened, so there is no frozen set to replay — it answers for the
	// program as it reads NOW, which is what the operator is about to run.
	x.pinned = pinnedProviderSet(derivePinnedProviders(wf, buildModelOverrides(spec.Launch.ModelOverrides), runFallbackEntries(spec.Launch.Fallback)))
	// The live resolution's own policy snapshot and tier probes (read-only
	// store reads, like every other stage here).
	x.policy = p.sharedTierPolicyFor(ctx)
	x.natives = map[string]*tierNative{
		"platform": p.newTierNative(ctx, "platform", secrets.PlatformOwnerKey, secrets.PlatformTenantID, previewBotID),
	}
	if x.orgID != "" {
		x.natives["org"] = p.newTierNative(ctx, "org", secrets.OrgTierOwnerKey(x.orgID), secrets.OrgTierTenantID(x.orgID), previewBotID)
	}
	wants, routes := wantsFor(wf, buildModelOverrides(spec.Launch.ModelOverrides), runFallbackEntries(spec.Launch.Fallback))
	for _, w := range wants {
		x.out.Pool.Wants = append(x.out.Pool.Wants, string(w.Source)+":"+w.Ref)
	}
	if !routes.NarrowSafe {
		x.out.Warnings = append(x.out.Warnings, "Some model routes are unresolved; the live resolver uses the full pool preference order.")
	}
	err := walkCredentialPlan(func() bool { return len(x.api)+len(x.oauth) > 0 }, func() bool { return x.poolGranted }, func(tier credentialTier, active bool) error {
		switch tier {
		case credentialTierBYOK:
			if err := x.apiStage("", spec.Context.TeamID, spec.OwnerID, usagecap.TenantScope(spec.Context.TeamID), previewBotID, spec.Launch.KeyOverrides, false, true); err != nil {
				return fmt.Errorf("credential metadata unavailable")
			}
		case credentialTierOAuth:
			x.oauthStage("user", spec.OwnerID, usagecap.TenantScope(spec.Context.TeamID), false, true)
			x.oauthStage("team", secrets.OrgOwnerKey(spec.Context.TeamID), usagecap.TenantScope(spec.Context.TeamID), false, true)
		case credentialTierOrg:
			if p.orgCredentialAudience(ctx, x.orgID, spec.Context.TeamID) {
				// The live tier's own order (the policy snapshot): a preview
				// filling in another order names a credential the launch
				// does not seal.
				x.policy.inOrder(func() {
					x.oauthStage("org", secrets.OrgTierOwnerKey(x.orgID), usagecap.OrgScope(x.orgID), true, active)
				}, func() {
					if err := x.apiStage("org", secrets.OrgTierTenantID(x.orgID), "", usagecap.OrgScope(x.orgID), previewBotID, nil, true, active); err != nil {
						x.warn("Org API-key metadata unavailable; selection is conditional.")
					}
				})
			} else {
				x.warn("Org tier not available to this team under the current audience or its metadata could not be read.")
			}
		case credentialTierPool:
			// The live walk's own filter: a provider a key sealed for its
			// routes already funds is not asked of the pool.
			poolWants := withoutFundedProviders(wants, func(prov string) bool {
				_, funded := x.pinnedAPI[secrets.Provider(prov)]
				return funded
			})
			if active && len(wants) > 0 && len(poolWants) == 0 {
				x.out.Pool.Reason = "routes_funded"
				break
			}
			if !active {
				poolWants = wants
			}
			x.out.Pool.Considered = active
			pool, err := p.credPool.Preview(ctx, credpool.Request{OrgID: x.orgID, TenantID: spec.Context.TeamID, UserID: spec.OwnerID, BotID: spec.Context.BotID, Wants: poolWants})
			if err != nil {
				x.out.Pool.Reason = "unknown"
				x.warn("Pool metadata unavailable; selection is conditional.")
				break
			}
			if active {
				x.out.Pool.Reason = string(pool.Reason)
			}
			for rank, d := range pool.Candidates {
				c := runview.CredentialPreviewCandidate{Tier: "pool", Rank: rank, Source: string(d.Source), Provider: d.Ref, Wire: secrets.WireFamily(d.Ref), Label: "Shared pool credential", State: string(d.Status), Reason: d.Reason, Conditional: true, Windows: []runview.CredentialPreviewWindow{}, Capacity: &runview.CredentialPreviewCapacity{LiveRuns: d.LiveRuns, MaxConcurrentRuns: d.MaxConcurrentRuns, RemainingUSD: d.RemainingUSD}}
				if !active {
					c.Selection = "not_consulted"
					c.Reason += " Pool is bypassed while any earlier credential is selected."
				}
				i := x.add(c)
				if d.Selected && active {
					x.poolGranted = true
					if d.Source == credpool.SourceOAuth {
						x.oauth[d.Ref] = i
					} else {
						x.api[secrets.Provider(d.Ref)] = i
					}
				}
			}
		case credentialTierPlatform:
			if p.platformAudienceAllows(ctx, "", x.orgID, spec.Context.TeamID) {
				x.policy.inOrder(func() {
					x.oauthStage("platform", secrets.PlatformOwnerKey, usagecap.ScopePlatform, true, active)
				}, func() {
					if err := x.apiStage("platform", secrets.PlatformTenantID, "", usagecap.ScopePlatform, previewBotID, nil, true, active); err != nil {
						x.warn("Platform API-key metadata unavailable; selection is conditional.")
					}
				})
			} else {
				x.warn("Platform tier not available to this team under the current audience or its metadata could not be read.")
			}
		case credentialTierRestore:
			// The live restore's own order: tier by tier as the walk visits
			// them, keys before forfaits within the tenant, the shared tiers'
			// fill order within each of those — and the facade policy.
			tierOf := func(i int) restoreTier {
				tier := x.out.Candidates[i].Tier
				return restoreTierOf(tier == "org", tier == "platform")
			}
			restoreKeys := func(tier restoreTier) {
				for _, provider := range allKnownProviders {
					i, ok := x.skippedAPI[provider]
					if !ok || tierOf(i) != tier {
						continue
					}
					outcome := sealDefault
					if tier == restoreTierTenant {
						if x.taken(string(provider)) {
							continue
						}
						// The live restore's clean replace: the team's own key
						// restored as the default supersedes a shared key
						// sealed for the same provider's routes.
						if j, held := x.pinnedAPI[provider]; held {
							delete(x.pinnedAPI, provider)
							x.out.Candidates[j].Reason += " Replaced by the team's own key, restored as the default."
						}
					} else if outcome = x.seal(x.out.Candidates[i].Tier, provider, true); outcome == sealNone {
						continue
					}
					if outcome == sealPinnedOnly {
						x.pinnedAPI[provider] = i
					} else {
						x.api[provider] = i
					}
					x.out.Candidates[i].State = "restored"
				}
			}
			restoreForfaits := func(tier restoreTier) {
				kinds := make([]string, 0, len(x.skippedOAuth))
				for kind := range x.skippedOAuth {
					kinds = append(kinds, kind)
				}
				sort.Strings(kinds)
				for _, kind := range kinds {
					if i := x.skippedOAuth[kind]; tierOf(i) == tier && !x.taken(kind) {
						x.oauth[kind] = i
						x.out.Candidates[i].State = "restored"
					}
				}
			}
			restoreKeys(restoreTierTenant)
			restoreForfaits(restoreTierTenant)
			for _, tier := range []restoreTier{restoreTierOrg, restoreTierPlatform} {
				x.policy.inOrder(func() { restoreForfaits(tier) }, func() { restoreKeys(tier) })
			}
		}
		return nil
	})
	if err != nil {
		return runview.CredentialPreview{}, err
	}
	wires := map[string][]string{}
	selectSlot := func(i int) {
		c := &x.out.Candidates[i]
		c.Selected = true
		c.Selection = "selected"
		wires[c.Wire] = append(wires[c.Wire], c.ID)
	}
	for _, provider := range allKnownProviders {
		if i, ok := x.api[provider]; ok {
			selectSlot(i)
		}
		if i, ok := x.pinnedAPI[provider]; ok {
			selectSlot(i)
			x.out.Candidates[i].RouteOnly = true
		}
	}
	kinds := make([]string, 0, len(x.oauth))
	for kind := range x.oauth {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		selectSlot(x.oauth[kind])
	}
	names := make([]string, 0, len(wires))
	for wire := range wires {
		names = append(names, wire)
	}
	sort.Strings(names)
	for _, wire := range names {
		x.out.Wires = append(x.out.Wires, runview.CredentialPreviewWire{Wire: wire, CandidateIDs: wires[wire]})
	}
	if len(wires) == 0 {
		x.warn("No database credential is predicted; runner environment availability is unknown.")
	}
	return x.out, nil
}

type credentialPreview struct {
	p     *Publisher
	ctx   context.Context
	orgID string
	out   runview.CredentialPreview
	api   map[secrets.Provider]int
	// pinnedAPI are the keys sealed for the routes naming their provider
	// only. Apart from api because they do not take their wire family: a
	// preview counting them there would keep a later forfait off a wire the
	// launch lets it fill.
	pinnedAPI     map[secrets.Provider]int
	oauth         map[string]int
	skippedAPI    map[secrets.Provider]int
	skippedOAuth  map[string]int
	accountGroups map[string]string
	poolGranted   bool
	// pinned are the providers a route of the previewed launch NAMES. A
	// shared tier may fund one of them even on a wire family another slot
	// already fills, so the preview has to apply the same exception or it
	// would report a credential the launch will grant as "not consulted".
	pinned map[string]bool
	// policy and natives mirror the live resolution's shared-tier ordering
	// and its per-tier "holds an Anthropic-native credential" probe.
	policy  sharedTierPolicy
	natives map[string]*tierNative
}

func (x *credentialPreview) warn(s string) { x.out.Warnings = append(x.out.Warnings, s) }
func (x *credentialPreview) add(c runview.CredentialPreviewCandidate) int {
	if c.Selection == "" {
		c.Selection = "shadowed"
	}
	c.ID = fmt.Sprintf("c%d", len(x.out.Candidates)+1)
	x.out.Candidates = append(x.out.Candidates, c)
	return len(x.out.Candidates) - 1
}

// seal is the preview's twin of the live key fill: the tenant stage seals
// every winner as the default; a shared stage skips a provider an earlier
// stage holds, then asks sealDecision with the preview's own view of the
// wire.
func (x *credentialPreview) seal(tier string, provider secrets.Provider, byWire bool) sealOutcome {
	if !byWire {
		return sealDefault
	}
	// A slot an earlier stage funded is not rewritten: the live fill never
	// even asks a shared tier for it.
	if _, held := x.api[provider]; held {
		return sealNone
	}
	if _, held := x.pinnedAPI[provider]; held {
		return sealNone
	}
	return sealDecision(provider, x.taken(string(provider)), x.pinned[strings.ToLower(string(provider))],
		func() bool { return x.policy.facadeMayDefault(x.natives[tier]) })
}
func (x *credentialPreview) taken(slot string) bool {
	wire := secrets.WireFamily(slot)
	for provider := range x.api {
		if secrets.WireFamily(string(provider)) == wire {
			return true
		}
	}
	for kind := range x.oauth {
		if secrets.WireFamily(kind) == wire {
			return true
		}
	}
	return false
}
func (x *credentialPreview) apiStage(tier, tenant, owner, meter, botID string, pins map[string]string, byWire, active bool) error {
	if x.p.apiKeys == nil {
		return nil
	}
	providers := append([]secrets.Provider(nil), allKnownProviders...)
	if len(providers) == 0 {
		return nil
	}
	ctx := store.WithTenant(x.ctx, tenant)
	visible, err := x.p.apiKeys.ListByTeam(ctx, tenant, owner)
	if err != nil {
		return err
	}
	overrides := map[secrets.Provider]string{}
	for provider, id := range pins {
		overrides[secrets.Provider(provider)] = id
	}
	winners := map[secrets.Provider]int{}
	first := map[secrets.Provider]int{}
	seen := map[string]bool{}
	ranks := map[secrets.Provider]int{}
	for _, candidate := range secrets.OrderedAPIKeyCandidates(visible, owner, providers, overrides) {
		k := candidate.Key
		if seen[k.ID] {
			continue
		}
		seen[k.ID] = true
		c := runview.CredentialPreviewCandidate{Tier: tier, Source: "api_key", Provider: string(k.Provider), Wire: secrets.WireFamily(string(k.Provider)), Rank: ranks[k.Provider], Pinned: candidate.Pinned, Label: k.Name, State: "unknown", Conditional: true, Windows: []runview.CredentialPreviewWindow{}}
		ranks[k.Provider]++
		if tier == "" {
			c.Tier = "team"
			if k.ScopeUserID == owner && owner != "" {
				c.Tier = "user"
			}
		}
		if c.Tier == "org" || c.Tier == "platform" {
			c.Label = "Shared " + c.Tier + " API key"
		}
		closed := x.windows(ctx, &c, usageBackendForProvider(k.Provider), meter, k.Fingerprint, false)
		if k.MaxConcurrentRuns > 0 {
			c.Capacity = &runview.CredentialPreviewCapacity{MaxConcurrentRuns: k.MaxConcurrentRuns}
			if x.p.store != nil && k.Fingerprint != "" {
				n, err := x.p.store.CountAliveRunsWithCredFingerprint(ctx, k.Fingerprint, "")
				if err != nil {
					if !closed {
						c.State = "unknown"
					}
					c.Reason += " Concurrency observation unavailable; this ceiling fails open."
				} else {
					c.Capacity.LiveRuns = &n
					if n >= k.MaxConcurrentRuns {
						closed = true
						if c.State != "blocked" {
							c.State = "at_capacity"
						}
						c.Reason += " API key concurrency ceiling reached."
					}
				}
			}
		}
		if candidate.Pinned && closed {
			c.Reason += " Explicit pin is honored even when blocked."
		}
		// The workload audience, shown rather than hidden: a key that vanishes
		// from the preview reads as a key that is gone. It is tracked apart
		// from `closed` because a pin does NOT lift it — the live resolver
		// reads it before the pin exemption, and a preview that promised
		// otherwise would be a confident lie about the next launch.
		outOfAudience := !k.ServesBotForLaunch(botID)
		if outOfAudience {
			// The route refuses an empty bot_id with a 400 before this runs
			// (pkg/server/credential_preview.go), so there is no "named no
			// bot" arm here: a branch that cannot execute reads as a
			// behaviour and is worse than its absence.
			c.State = string(credpool.StatusBotFiltered)
			c.Reason += " This key's workload audience does not name bot " + botID + "."
		}
		if !active || x.seal(tier, k.Provider, byWire) == sealNone {
			c.Selection = "not_consulted"
			c.Reason += " This tier is bypassed for this wire in the current launch."
			_, heldDefault := x.api[k.Provider]
			_, heldRoute := x.pinnedAPI[k.Provider]
			switch {
			case !active:
			case byWire && (heldDefault || heldRoute):
				c.Reason += " An earlier credential already funds " + string(k.Provider) + "."
			case isFacadeProvider(k.Provider) && !x.taken(string(k.Provider)):
				c.Reason += " A " + string(k.Provider) + " key is kept off the anthropic wire's default here (facade_default=" + string(x.policy.facade) + ")."
			}
		}
		i := x.add(c)
		// `first` feeds the restore stage, which hands a skipped candidate the
		// slot when nothing else filled the wire. A key excluded by its
		// audience must stay out of that too, or the preview would predict
		// exactly the hand-out the live resolver refuses.
		if _, ok := first[k.Provider]; !ok && !outOfAudience {
			first[k.Provider] = i
		}
		if _, ok := winners[k.Provider]; !ok && !outOfAudience && (!closed || candidate.Pinned) {
			winners[k.Provider] = i
		}
	}
	if !active {
		return nil
	}
	// Resolve chooses independently per provider. Shared tiers then fill in the
	// same fixed provider order as fillFromOrg/fillFromPlatform, not map order.
	for _, provider := range providers {
		outcome := x.seal(tier, provider, byWire)
		if i, ok := winners[provider]; ok {
			switch outcome {
			case sealDefault:
				x.api[provider] = i
			case sealPinnedOnly:
				x.pinnedAPI[provider] = i
			}
		} else if i, ok := first[provider]; ok && outcome != sealNone {
			// Remembered for the restore only when the live fill would have
			// tried it: a key the policy withholds is not a refused one.
			if _, seen := x.skippedAPI[provider]; !seen {
				x.skippedAPI[provider] = i
			}
		}
	}
	return nil
}
func (x *credentialPreview) oauthStage(tier, owner, meter string, byWire, active bool) {
	if x.p.oauthForfait == nil || owner == "" {
		return
	}
	records, err := x.p.oauthForfait.ListByUser(x.ctx, owner)
	if err != nil {
		x.warn(tier + " subscription metadata unavailable; selection is conditional.")
		return
	}
	for _, rec := range records {
		kind := string(rec.Kind)
		// Keep lower ranks and shared tiers visible for explaining fallbacks;
		// a funded wire only prevents their selection, not their observation.
		c := runview.CredentialPreviewCandidate{Tier: tier, Source: "oauth", Provider: kind, Wire: secrets.WireFamily(kind), Rank: rec.Rank, Label: rec.AccountLabel, State: "unknown", Conditional: true, Windows: []runview.CredentialPreviewWindow{}}
		if c.Label == "" {
			c.Label = kind + " subscription"
		}
		if tier == "org" || tier == "platform" {
			c.Label = "Shared " + tier + " subscription"
		}
		if secrets.IsAccountFingerprint(rec.Fingerprint) {
			group, ok := x.accountGroups[rec.Fingerprint]
			if !ok {
				group = fmt.Sprintf("account%d", len(x.accountGroups)+1)
				x.accountGroups[rec.Fingerprint] = group
			}
			c.AccountGroup = group
		}
		closed := x.windows(x.ctx, &c, usageBackendForKind(rec.Kind), meter, rec.Fingerprint, true)
		if !active || byWire && x.taken(kind) {
			c.Selection = "not_consulted"
			c.Reason += " This tier is bypassed for this wire in the current launch."
		}
		i := x.add(c)
		if !active {
			continue
		}
		if _, ok := x.oauth[kind]; ok {
			continue
		}
		if byWire && x.taken(kind) {
			continue
		}
		if closed {
			if _, ok := x.skippedOAuth[kind]; !ok {
				x.skippedOAuth[kind] = i
			}
			continue
		}
		x.oauth[kind] = i
	}
}

// windows uses the same cached refusal/cap evaluation and probe predicates as
// launch. A needed probe is reported, never executed by an observation.
func (x *credentialPreview) windows(ctx context.Context, c *runview.CredentialPreviewCandidate, backend, meter, fp string, oauth bool) bool {
	if x.p.usageCaps == nil || backend == "" || fp == "" {
		c.Reason = "No usage-window observation is available; the launch assumes availability."
		return false
	}
	lctx, cancel := context.WithTimeout(ctx, usageCapLookupTimeout)
	defer cancel()
	key := usagecap.Key(backend, meter, fp)
	readings, err := x.p.usageCaps.Latest(lctx, key)
	if err != nil {
		c.Reason = "Usage-window observation unavailable; the launch fails open."
		return false
	}
	fresh := false
	for _, r := range readings {
		f := r.Fresh(x.out.ObservedAt, x.p.trust.Normalized())
		fresh = fresh || f
		w := runview.CredentialPreviewWindow{Name: string(r.Window), Status: r.Status, ObservedAt: r.ObservedAt, Fresh: f}
		if r.Utilization != 0 || r.Status != usagecap.StatusRejected {
			v := r.Percent()
			w.Percent = &v
		}
		if !r.ResetsAt.IsZero() {
			at := r.ResetsAt
			w.ResetsAt = &at
		}
		c.Windows = append(c.Windows, w)
	}
	until, why := x.p.evaluateCredentialWindows(ctx, readings, x.out.ObservedAt)
	if !until.IsZero() {
		c.State = "blocked"
		c.Reason = why
		c.ReopensAt = &until
		return true
	}
	// The same decision the launch makes, read through one predicate: a
	// preview that announced a probe the launch then skipped would be
	// telling the operator about work that never happens.
	if oauth && x.p.usageProbe != nil && (x.p.initialProbeWanted(key, fp, readings, x.out.ObservedAt) || x.p.staleReadingsSuggestClosed(ctx, readings, x.out.ObservedAt)) {
		c.State = "probe_required"
		c.Reason = "Launch will probe this new or stale account; its availability is unknown until then."
		return false
	}
	if fresh {
		c.State = "available"
		c.Reason = "No fresh window refusal or operator cap blocks this credential."
	} else {
		c.State = "unknown"
		c.Reason = "No fresh usage-window observation; the launch assumes availability."
	}
	return false
}
