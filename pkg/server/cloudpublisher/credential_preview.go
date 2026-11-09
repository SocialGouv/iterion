package cloudpublisher

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/llmroute"
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
		api: map[secrets.Provider]int{}, pinnedAPI: map[secrets.Provider]int{}, oauth: map[string]int{}, skippedAPI: map[secrets.Provider][]int{}, skippedOAuth: map[string][]int{}, accountGroups: map[string]string{}, floorHeld: map[int]bool{},
	}
	x.orgID = p.orgIDForTeam(ctx, spec.Context.TeamID)
	// The same canonicalisation the live walk applies, or the preview would
	// answer for a spelling the launch never uses.
	previewBotID := botregistry.NormalizeName(spec.Context.BotID)
	// The launch view reads the run's vars by provenance, exactly as the
	// live launch will (the parity doctrine): declared defaults narrow the
	// gate, launch values only where the spec pins them, a late name leaves
	// the route undecided (ADR-129 §4).
	previewView := model.RouteViewForLaunch(wf, buildModelOverrides(spec.Launch.ModelOverrides), runFallbackEntries(spec.Launch.Fallback), spec.Launch.Vars, spec.Launch.TrustedVarNames)
	// The same allowlist the live launch will enforce (the parity doctrine):
	// the preview's pinned set, pool wants and candidates read the PRUNED
	// set, so it never shows a credential for a provider the tenant forbids
	// (ADR-129 §5).
	previewView.AllowedProviders = policyProviders(spec.LLMRoutePolicy)
	// The same derivation the launch stamps on the run document. Derived
	// fresh here on purpose: a preview describes a launch that has not
	// happened, so there is no frozen set to replay — it answers for the
	// program as it reads NOW, which is what the operator is about to run.
	x.pinned = pinnedProviderSet(derivePinnedProviders(previewView))
	// Same predicate as the live fill (#2038): an env-funded run acquires no
	// LLM credential, so the preview shows no candidate and reads no tier.
	envFunded := wf != nil && model.EffectiveProviders(previewView, knownPoolProviders).OnlyEnvFunded()
	// The live resolution's own policy snapshot and tier probes (read-only
	// store reads, like every other stage here) — and the launch's
	// RESOLVED routing policy whitelists the slots exactly as the launch
	// will (the parity doctrine: fill, restore and preview apply the same
	// answer).
	x.policy = p.sharedTierPolicyFor(ctx)
	if spec.LLMRoutePolicy != nil {
		// No refusal record: a preview is not a walk, and it names every
		// exclusion per candidate already (exclusionReason).
		x.policy = x.policy.withSlotWhitelist(spec.LLMRoutePolicy.PairOrder, nil)
		// The tenant's provider allowlist joins the same gate the live walk
		// runs (the parity doctrine): a slot whose provider the list excludes
		// is sealed by NO tier, so no candidate names a credential the live
		// launch would refuse to acquire (ADR-129 §5).
		if allowed := model.AllowedProviderSet(policyProviders(spec.LLMRoutePolicy)); allowed != nil {
			x.policy = x.policy.withProviderAllowlist(allowed)
			x.allowed = allowed
		}
	}
	// The team's credential-fallback policy, the same fresh read the launch
	// applies — UNCONDITIONALLY, like the launch: a failed or invalid read
	// refuses even an env-funded run there, so the preview warns on it
	// everywhere. The preview never refuses (it observes), so a failed read
	// walks the shared tiers with a warning saying the live launch is the
	// one that decides; "none" only shapes the shared tiers below.
	sovereignPreview := false
	policy, ferr := p.llmFallbackForTeam(ctx, spec.Context.TeamID)
	switch {
	case ferr != nil:
		x.warn("The team's llm_fallback policy could not be read; the live launch refuses on this error and this preview is conditional.")
	case policy == identity.LLMFallbackNone:
		sovereignPreview = true
		if !envFunded {
			x.warn("The team runs with llm_fallback=none: the shared tiers (org, pool, platform) are not consulted, and a route nothing of the team's own funds refuses the launch.")
		}
	}
	// The team's runner-pool mapping, through the launch's own reader (D13):
	// a pool team's launch consults NO shared tier — a pool pod would hold
	// other tenants' credentials — so a preview blind to the mapping predicts
	// fills the launch never acquires, and the verdict it mirrors below is
	// computed on them. Read like the policy above: unconditionally, and a
	// failed read warns instead of refusing, since the preview observes.
	pooledPreview := false
	runnerPool, perr := p.runnerPoolForTeam(ctx, spec.Context.TeamID)
	switch {
	case perr != nil:
		x.warn("The team's runner-pool mapping could not be read; the live launch refuses on this error and this preview is conditional.")
	case runnerPool != "":
		pooledPreview = true
	}
	// The launch's own `runnerPool != "" || sovereign`: ONE expression for the
	// two postures that block the shared tiers, read by every site below that
	// mirrors one — the tier walk, the fallback door, and therefore the
	// unfunded verdict those fills feed.
	sharedTiersBlocked := sovereignPreview || pooledPreview
	if envFunded {
		x.out.Warnings = append(x.out.Warnings, "Every model route of this run rides the runner's openai_compatible gateway: no credential is acquired for it.")
	} else {
		if pooledPreview {
			x.warn(fmt.Sprintf("The team is mapped to runner pool %q: the shared tiers (org, pool, platform) are not consulted — only the team's own credentials fund this run.", runnerPool))
		}
		tenantOwners := []string{spec.OwnerID}
		if spec.Context.TeamID != "" {
			tenantOwners = append(tenantOwners, secrets.OrgOwnerKey(spec.Context.TeamID))
		}
		x.natives = map[string]*tierNative{
			"tenant":   p.newTierNative(ctx, "tenant", spec.Context.TeamID, previewBotID, tenantOwners...),
			"platform": p.newTierNative(ctx, "platform", secrets.PlatformTenantID, previewBotID, secrets.PlatformOwnerKey),
		}
		if x.orgID != "" {
			x.natives["org"] = p.newTierNative(ctx, "org", secrets.OrgTierTenantID(x.orgID), previewBotID, secrets.OrgTierOwnerKey(x.orgID))
		}
		// Same answer as the live fill: `auto` asks the RUN — the team's own
		// credentials, the org tier's and the platform tier's — so the preview
		// cannot promise a facade default the fill will refuse, or refuse one it
		// will serve.
		x.policy.runNative = orNative(orNative(x.natives["tenant"], x.natives["org"]), x.natives["platform"])
	}
	wants, routes := wantsFor(previewView)
	for _, w := range wants {
		x.out.Pool.Wants = append(x.out.Pool.Wants, string(w.Source)+":"+w.Ref)
	}
	if !routes.NarrowSafe {
		x.out.Warnings = append(x.out.Warnings, "Some model routes are unresolved; the live resolver uses the full pool preference order.")
	}
	// The allowlist's refusals are the live launch's, observed here (the
	// preview never refuses): a primary outside the list, or a route the
	// list cannot check, refuses that launch by name (ADR-129 §5).
	if site, prov, ok := model.FirstForbiddenPrimary(routes); ok {
		level := ""
		if spec.LLMRoutePolicy != nil {
			level = spec.LLMRoutePolicy.Sources[llmroute.FieldProviders]
		}
		x.out.Warnings = append(x.out.Warnings, fmt.Sprintf("The live launch refuses: node %s pins provider %q outside the providers allowlist (set by the %s level) — refused, never re-routed.", site, prov, level))
	}
	if u := model.FirstUnreadable(routes); u != nil {
		x.out.Warnings = append(x.out.Warnings, fmt.Sprintf("The live launch refuses: route %s cannot be checked against the providers allowlist (%s unreadable); resolve the field or launch without the providers list.", u.Site, u.Field))
	}
	// A rescue outside the list is PRUNED, not refused — the launch proceeds
	// without it, so the preview names what the tenant's own statement took
	// away: the second server this run will not have (ADR-129 §5).
	for _, prov := range sortedForbiddenProviders(routes.Forbidden) {
		x.out.Warnings = append(x.out.Warnings, fmt.Sprintf("The live launch prunes the %s route at %s: provider %q is outside the providers allowlist — no credential is acquired for it.", prov, strings.Join(routes.Forbidden[prov], ", "), prov))
	}
	var err error
	if envFunded {
		// Nothing to walk: no stage acquires anything for this run.
	} else if err = walkCredentialPlan(func() bool { return len(x.api)+len(x.oauth) > 0 }, func() bool { return x.poolGranted }, func() bool { return spec.PoolAttachment != nil && spec.PoolAttachment.PoolID != "" }, func(tier credentialTier, active bool) error {
		switch tier {
		case credentialTierBYOK:
			if err := x.apiStage("", spec.Context.TeamID, spec.OwnerID, usagecap.TenantScope(spec.Context.TeamID), previewBotID, spec.Launch.KeyOverrides, false, true); err != nil {
				return fmt.Errorf("credential metadata unavailable")
			}
		case credentialTierOAuth:
			x.oauthStage("user", spec.OwnerID, usagecap.TenantScope(spec.Context.TeamID), false, true)
			x.oauthStage("team", secrets.OrgOwnerKey(spec.Context.TeamID), usagecap.TenantScope(spec.Context.TeamID), false, true)
		case credentialTierOrg:
			if sharedTiersBlocked {
				x.warn("Org tier not consulted: " + blockedPosture(sovereignPreview, runnerPool) + ".")
				return nil
			}
			if p.orgCredentialAudience(ctx, x.orgID, spec.Context.TeamID) {
				// The live tier's own order (the policy snapshot): a preview
				// filling in another order names a credential the launch
				// does not seal.
				x.policy.inOrder(func() {
					x.oauthStage("org", secrets.OrgTierOwnerKey(x.orgID), usagecap.OrgScope(x.orgID), true, active)
				}, func() {
					if err := x.apiStage("org", secrets.OrgTierTenantID(x.orgID), "", usagecap.OrgScope(x.orgID), previewBotID, nil, true, active); err != nil {
						x.conditional("Org API-key metadata unavailable; selection is conditional.")
					}
				})
			} else {
				x.warn("Org tier not available to this team under the current audience or its metadata could not be read.")
			}
		case credentialTierPool:
			if sharedTiersBlocked {
				x.warn("Pool not consulted: " + blockedPosture(sovereignPreview, runnerPool) + ".")
				return nil
			}
			// The ATTACHED pool (ADR-164): the preview answers exactly what
			// the live walk will draw — same named pool, same want order
			// (the gateway want enters only through the attachment), same
			// filled-slot gate (law 1), same margin predicate constructor
			// (law 4's parity).
			if spec.PoolAttachment != nil && spec.PoolAttachment.PoolID != "" {
				attachedWants := wants
				if !routes.NarrowSafe || slices.Contains(routes.Providers, string(secrets.ProviderOpenAICompatible)) {
					attachedWants = append(append([]credpool.Credential{}, attachedWants...), credpool.Credential{Source: credpool.SourceAPIKey, Ref: string(secrets.ProviderOpenAICompatible)})
				}
				var marginFn func(credpool.Pledge) (time.Time, bool)
				if slices.Contains(spec.PoolAttachment.Strategy.Triggers, "usage_window") {
					marginFn = p.closedAtMargin(ctx, strategyMargin(spec.PoolAttachment.Strategy))
				}
				// Law 1's gate, exactly as the live walk: a slot tiers 1–4
				// sealed (or the whitelist holds out) is not asked of the
				// pool — a candidate the launch would never draw must not
				// read as one it might.
				asked := make([]credpool.Credential, 0, len(attachedWants))
				for _, w := range attachedWants {
					_, filled := false, false
					switch w.Source {
					case credpool.SourceOAuth:
						_, filled = x.oauth[w.Ref]
					case credpool.SourceAPIKey:
						_, filled = x.api[secrets.Provider(w.Ref)]
					}
					if filled {
						continue
					}
					slot := ""
					switch w.Source {
					case credpool.SourceOAuth:
						slot = slotOfKind(w.Ref)
					case credpool.SourceAPIKey:
						slot = slotOfProvider(secrets.Provider(w.Ref))
					}
					if x.policy.sealableSlot(slot) {
						asked = append(asked, w)
					}
				}
				x.out.Pool.Considered = active
				pool, err := p.credPool.Preview(ctx, credpool.Request{
					OrgID: x.orgID, TenantID: spec.Context.TeamID, UserID: spec.OwnerID, BotID: spec.Context.BotID,
					Wants: asked, PoolIDs: []string{spec.PoolAttachment.PoolID},
					RankMode: credpool.StrategyMode(spec.PoolAttachment.Strategy.Mode), ClosedAtMargin: marginFn,
				})
				if err != nil {
					x.out.Pool.Reason = "unknown"
					x.conditional("Attached pool metadata unavailable; selection is conditional.")
					break
				}
				if active {
					x.out.Pool.Reason = string(pool.Reason)
				}
				for rank, d := range pool.Candidates {
					c := runview.CredentialPreviewCandidate{Tier: "pool", Rank: rank, Source: string(d.Source), Provider: d.Ref, Wire: secrets.WireFamily(d.Ref), Label: "Key pool " + spec.PoolAttachment.PoolID, State: string(d.Status), Reason: d.Reason, Conditional: true, Windows: []runview.CredentialPreviewWindow{}, Capacity: &runview.CredentialPreviewCapacity{LiveRuns: d.LiveRuns, MaxConcurrentRuns: d.MaxConcurrentRuns, RemainingUSD: d.RemainingUSD}}
					if d.Source == credpool.SourceAPIKey && d.Ref == string(secrets.ProviderOpenAICompatible) {
						c.Label = "Key pool gateway entry (paired key+URL)"
					}
					if d.Status == credpool.StatusActive && d.Source == credpool.SourceAPIKey && d.Ref != string(secrets.ProviderOpenAICompatible) {
						c.Reason = strings.TrimSpace(c.Reason + " reactive — no provider telemetry; gates on its provider cooldown alone.")
					}
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
				break
			}
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
				x.conditional("Pool metadata unavailable; selection is conditional.")
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
			if sharedTiersBlocked {
				x.warn("Platform tier not consulted: " + blockedPosture(sovereignPreview, runnerPool) + ".")
				return nil
			}
			if p.platformAudienceAllows(ctx, "", x.orgID, spec.Context.TeamID) {
				x.policy.inOrder(func() {
					x.oauthStage("platform", secrets.PlatformOwnerKey, usagecap.ScopePlatform, true, active)
				}, func() {
					if err := x.apiStage("platform", secrets.PlatformTenantID, "", usagecap.ScopePlatform, previewBotID, nil, true, active); err != nil {
						x.conditional("Platform API-key metadata unavailable; selection is conditional.")
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
			// The live restore's deferral: a team key whose provider's routes
			// a shared tier already funded waits for the shared tiers.
			var deferredTenantKeys []deferredPreviewKey
			// The live restore's candidate walk: a credential-level refusal
			// (the reserve holds THIS one) takes the next candidate of the
			// slot, a slot-level one ends the slot — exactly the two kinds
			// restoreKeys in publisher.go distinguishes.
			restoreKeys := func(tier restoreTier) {
				for _, provider := range allKnownProviders {
					for _, i := range x.skippedAPI[provider] {
						if tierOf(i) != tier {
							continue
						}
						if x.floorHeld[i] {
							x.out.Candidates[i].Reason += " Not restored: the reserve holds it for another workload — the credential is usable, it is spoken for."
							continue
						}
						outcome := sealDefault
						if tier == restoreTierTenant {
							if x.taken(string(provider)) {
								break
							}
							if _, held := x.pinnedAPI[provider]; held {
								deferredTenantKeys = append(deferredTenantKeys, deferredPreviewKey{prov: provider, i: i})
								break
							}
						} else if outcome = x.seal(x.out.Candidates[i].Tier, provider, true); outcome == sealNone {
							break
						}
						// refused_pinned_key (#1999): the same judge the live
						// restore asks — under `forfait` a refused key stays out
						// of a family another credential holds; the preview says
						// so on the candidate instead of predicting a restore.
						if outcome == sealPinnedOnly && x.policy.restoreRefusedKey(provider, x.taken(string(provider))) {
							x.out.Candidates[i].Reason += " Not restored: refused_pinned_key=forfait keeps a refused key out of the family another credential holds — the routes naming its provider spend that holder."
							break
						}
						if outcome == sealPinnedOnly {
							x.pinnedAPI[provider] = i
						} else {
							x.api[provider] = i
						}
						x.out.Candidates[i].State = "restored"
						break
					}
				}
			}
			restoreForfaits := func(tier restoreTier) {
				kinds := make([]string, 0, len(x.skippedOAuth))
				for kind := range x.skippedOAuth {
					kinds = append(kinds, kind)
				}
				sort.Strings(kinds)
				for _, kind := range kinds {
					for _, i := range x.skippedOAuth[kind] {
						if tierOf(i) != tier {
							continue
						}
						if x.taken(kind) {
							break
						}
						if x.floorHeld[i] {
							x.out.Candidates[i].Reason += " Not restored: the reserve holds it for another workload — the credential is usable, it is spoken for."
							continue
						}
						x.oauth[kind] = i
						x.out.Candidates[i].State = "restored"
						break
					}
				}
			}
			restoreKeys(restoreTierTenant)
			restoreForfaits(restoreTierTenant)
			for _, tier := range []restoreTier{restoreTierOrg, restoreTierPlatform} {
				x.policy.inOrder(func() { restoreForfaits(tier) }, func() { restoreKeys(tier) })
			}
			// The last park point, as live: only into a family no tier
			// refilled and some route may read the default of, and then over
			// the shared route key.
			for _, deferred := range deferredTenantKeys {
				provider, i := deferred.prov, deferred.i
				if x.taken(string(provider)) {
					x.out.Candidates[i].Reason += " Not restored: a shared key serves the routes naming its provider, and another credential holds the wire."
					continue
				}
				if !readsWireDefault(routes, provider) {
					x.out.Candidates[i].Reason += " Not restored: a shared key serves the routes naming its provider, and no route reads the wire's default."
					continue
				}
				if j, held := x.pinnedAPI[provider]; held {
					delete(x.pinnedAPI, provider)
					x.out.Candidates[j].Reason += " Replaced by the team's own key, restored as the default."
				}
				x.api[provider] = i
				x.out.Candidates[i].State = "restored"
			}
		}
		return nil
	}); err != nil {
		return runview.CredentialPreview{}, err
	}
	// The unfunded refusals are the live launch's, observed here BY NAME (the
	// preview never refuses): the SAME arming, the SAME decision and the
	// launch's own sentence, read on the fills this preview predicts instead
	// of a sealed bundle — so an operator cannot preview a run that starts
	// and launch one the publisher refuses for a provider nothing funds
	// (#2485). Placed after the tier walk because the verdict needs those
	// fills; the SET comes through attributedRoutes, the launch's own seam,
	// so a provider family that stops being attributed stops being named on
	// both surfaces at once. A set or a sentence rebuilt here is how a view
	// starts contradicting the decision it observes.
	//
	// And the set is read on the program the launch JUDGES, not on the one
	// its author wrote: the launch reads it AFTER the policy ladder landed
	// its held rungs, and a rung's own provider is a route the run may really
	// spend — which is exactly what the knob's rule counts. Composed through
	// the launch's own composer over the fills predicted here, and RESTORED
	// immediately: the live fallback door runs BEFORE the ladder, so the door
	// block below must answer on the authored program, as it does live.
	restoreRoutes := snapshotFallbackRoutes(wf)
	composePolicyLadder(wf, previewView, spec.LLMRoutePolicy, model.AllowedProviderSet(previewView.AllowedProviders), spec.Launch.Vars, doorServesMetadata(x), nil)
	previewAttributed, previewSites := attributedRoutes(previewView)
	restoreRoutes()
	refusal := unfundedRefusalFor(spec.Context.TeamID, previewAttributed, previewSites, metadataFunding(x), metadataHoldsNothing(x), p.unfundedPostureFor(wf, sovereignPreview, envFunded))
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
	switch {
	case refusal != nil:
		// A refused launch publishes nothing, so whether the runner's
		// environment could have funded it is a question the launch never
		// asks: the verdict replaces the "unknown" line, it does not sit
		// beside it. And the verdict is a FACT only on fills every tier
		// answered for — a tier whose metadata could not be read may hold
		// the very credential that funds a route named here, so the mirror
		// then says what it could not see instead of stating the refusal.
		if x.fillsConditional {
			x.warn("The live launch refuses unless a tier whose metadata was unavailable funds the routes named here: " + refusal.Error())
		} else {
			x.warn("The live launch refuses: " + refusal.Error())
		}
	case len(wires) == 0:
		x.warn("No database credential is predicted; runner environment availability is unknown.")
	}
	// The routing fallback door's state (ADR-121 § Delivery 2): computed
	// OUTSIDE the tier walk by the same derivation the live consult runs
	// (doorWantsFor — the difference is only the serves argument, the
	// preview's metadata assuming materialization). The pool block above
	// stays bypassed for exactly these runs by the plan, so the door
	// answers in its own fields; when armed, a second read-only Preview
	// with FallbackOnly names what the donor-consented candidates look
	// like. Same arm conditions as the live door: a run that holds its
	// own credential, no whole-bundle grant predicted, not env-funded,
	// and a policy whose ladder can dispatch a missing kind.
	if !envFunded && len(x.api)+len(x.oauth) > 0 && !x.poolGranted &&
		spec.LLMRoutePolicy != nil && len(spec.LLMRoutePolicy.Triggers) > 0 && len(spec.LLMRoutePolicy.PairOrder) > 0 && p.credPool != nil && !sharedTiersBlocked {
		wants, _, ok := doorWantsFor(spec.LLMRoutePolicy, previewView, spec.Launch.Vars, doorServesMetadata(x))
		if !ok {
			x.out.Pool.DoorReason = "no_dispatchable_wants"
		} else {
			doorWants := make([]string, 0, len(wants))
			for _, w := range wants {
				doorWants = append(doorWants, string(w.Source)+":"+w.Ref)
			}
			x.out.Pool.DoorWants = doorWants
			door, derr := p.credPool.Preview(ctx, credpool.Request{OrgID: x.orgID, TenantID: spec.Context.TeamID, UserID: spec.OwnerID, BotID: spec.Context.BotID, Wants: wants, FallbackOnly: true})
			if derr != nil {
				x.out.Pool.DoorReason = "unknown"
				x.warn("Fallback-door pool metadata unavailable; selection is conditional.")
			} else if string(door.Reason) == "" {
				// A healthy pool that would serve: the broker leaves its
				// reason empty on success — the preview cannot grant, say
				// what it WOULD serve, in the door_wants spelling.
				x.out.Pool.DoorReason = "would_serve:" + string(wants[0].Source) + ":" + wants[0].Ref
			} else {
				x.out.Pool.DoorReason = "consulted:" + string(door.Reason)
			}
		}
	}
	return x.out, nil
}

// blockedPosture names, for an operator's remedy, which of the two postures
// keeps the shared tiers out of this run — they can hold at once, and they
// are not remedied the same way: a policy is the team's to change, a pool
// mapping the platform's.
func blockedPosture(sovereign bool, runnerPool string) string {
	reasons := make([]string, 0, 2)
	if sovereign {
		reasons = append(reasons, "the team runs with llm_fallback=none")
	}
	if runnerPool != "" {
		reasons = append(reasons, fmt.Sprintf("the team is mapped to runner pool %q", runnerPool))
	}
	return strings.Join(reasons, ", and ")
}

// doorServesMetadata is the preview's answer to doorServes: the same
// held-slot predicate over the metadata that assumes materialization.
func doorServesMetadata(x *credentialPreview) func(_, credential string) bool {
	return func(_, credential string) bool {
		for prov := range x.api {
			if slotOfProvider(prov) == credential {
				return true
			}
		}
		for prov := range x.pinnedAPI {
			if slotOfProvider(prov) == credential {
				return true
			}
		}
		for kind := range x.oauth {
			if slotOfKind(kind) == credential {
				return true
			}
		}
		return false
	}
}

// metadataHoldsNothing is `!holdsLLMCredential(bundle)` on the fills this
// preview PREDICTS: the sovereign refusal of a run nothing of the team's
// funds and no route of which the walk can attribute reads it, and it is the
// same twinning as metadataFunding below — a different SOURCE for the same
// question, never a different question.
func metadataHoldsNothing(x *credentialPreview) bool {
	return len(x.api)+len(x.pinnedAPI)+len(x.oauth) == 0
}

// metadataFunding is bundleFunding on the fills this preview PREDICTS: what
// separates the two surfaces is the funding they can SEE (a sealed bundle
// against predicted candidates, the doorServes / doorServesMetadata motif),
// never a second reading of what funded means.
func metadataFunding(x *credentialPreview) map[string]bool {
	slots := make([]secrets.Provider, 0, len(x.api)+len(x.pinnedAPI))
	for prov := range x.api {
		slots = append(slots, prov)
	}
	// Pinned keys fund the routes that name their provider — the very routes
	// the refusal is about — so they count here exactly as the launch counts
	// them (fundedAPIKeySlots reads both maps).
	for prov := range x.pinnedAPI {
		slots = append(slots, prov)
	}
	kinds := make([]string, 0, len(x.oauth))
	for kind := range x.oauth {
		kinds = append(kinds, kind)
	}
	return fundedProviderSet(slots, kinds)
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
	pinnedAPI map[secrets.Provider]int
	oauth     map[string]int
	// skippedAPI / skippedOAuth are EVERY candidate the live fill passed
	// over, per slot and in fill order — the walk's own memory shape
	// (skipped_creds.go). One index per slot would make the preview predict
	// "nothing comes back" for a slot whose reserved candidate the walk now
	// walks past to its dead sibling.
	skippedAPI    map[secrets.Provider][]int
	skippedOAuth  map[string][]int
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
	// allowed is the run's provider allowlist (ADR-129 §5) as the policy's
	// joined gate holds it: kept beside the policy so a candidate's
	// exclusion reason can name the gate that actually excluded it — the
	// allowlist refusing the slot's provider, or the routing pair order.
	allowed map[string]bool
	// floorHeld are the candidates a budget-floor reservation passed over:
	// usable, but spoken for by another workload. The live restore never
	// hands one back (skipTracker.floorHeld), so the preview's must not either.
	floorHeld map[int]bool
	// fillsConditional records that a tier's metadata could not be read: the
	// fills are then UNDER-predicted (a credential that tier holds is not
	// among them), and the unfunded verdict computed on them is a guess the
	// mirror must say is one.
	fillsConditional bool
}

func (x *credentialPreview) warn(s string) { x.out.Warnings = append(x.out.Warnings, s) }

// conditional is warn for a tier read that failed — ONE chokepoint for the
// class, so every degraded fill reaches the mirror's qualifier. A site that
// warned through warn alone would leave the mirror stating a definitive
// refusal on fills it never saw whole. The fallback door's degraded read is
// not of this class: it is consulted after the verdict and fills nothing.
func (x *credentialPreview) conditional(s string) {
	x.fillsConditional = true
	x.warn(s)
}
func (x *credentialPreview) add(c runview.CredentialPreviewCandidate) int {
	if c.Selection == "" {
		c.Selection = "shadowed"
	}
	c.ID = fmt.Sprintf("c%d", len(x.out.Candidates)+1)
	x.out.Candidates = append(x.out.Candidates, c)
	return len(x.out.Candidates) - 1
}

// exclusionReason names the gate that excluded a credential slot, through
// the ONE judgement every message prescribing a remedy asks (excluderOfSlot):
// the tenant's providers allowlist, or the launch's routing pair order.
func (x *credentialPreview) exclusionReason(slot string) string {
	if excluderOfSlot(x.allowed, slot) == excludedByProviderAllowlist {
		return " Outside this launch's providers allowlist — the live launch seals no credential for it."
	}
	return " Outside the launch's routing pair order — the live launch seals no credential for it."
}

// seal is the preview's twin of the live key fill: the tenant stage seals
// every winner as the default; a shared stage skips a provider an earlier
// stage holds, then asks sealDecision with the preview's own view of the
// wire.
func (x *credentialPreview) seal(tier string, provider secrets.Provider, byWire bool) sealOutcome {
	// The launch's own gates bound every tier the preview walks (the parity
	// doctrine): a slot the routing policy's whitelist or the tenant's
	// provider allowlist excludes is sealed by no tier — the same predicate
	// the live key fill consults before it seals.
	if !x.policy.sealableSlot(slotOfProvider(provider)) {
		return sealNone
	}
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
	// first holds EVERY candidate of a provider the live fill would have
	// tried, in walk order: the restore stage below walks them like the live
	// one, so a candidate the reserve holds does not stand in for its
	// siblings.
	first := map[secrets.Provider][]int{}
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
		closed, floorHeld := x.windows(ctx, &c, usageBackendForProvider(k.Provider), meter, k.Fingerprint, false)
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
		// Each audience is named by the one that refused — the live walk
		// reports the cause beside the key (secrets.AudienceRefusal), and the
		// preview says the same thing, or the operator edits the wrong list.
		launchRepo := secrets.LaunchRepoFromContext(ctx)
		outOfBot, outOfRepo := !k.ServesBotForLaunch(botID), !k.ServesRepoForLaunch(launchRepo)
		outOfAudience := outOfBot || outOfRepo
		if outOfBot {
			// The route refuses an empty bot_id with a 400 before this runs
			// (pkg/server/credential_preview.go), so there is no "named no
			// bot" arm here: a branch that cannot execute reads as a
			// behaviour and is worse than its absence.
			c.State = string(credpool.StatusBotFiltered)
			c.Reason += " This key's workload audience does not name bot " + botID + "."
		}
		if outOfRepo {
			if !outOfBot {
				c.State = string(credpool.StatusRepoFiltered)
			}
			// The key's own list stays out of the reason, as the bot list
			// does: a shared-tier key belongs to another tenant, and the
			// repositories it names are theirs to know. The server log
			// carries the list, for the operator.
			c.Reason += " This key's repository audience does not name " + previewLaunchRepo(launchRepo) + "."
		}
		if !active || x.seal(tier, k.Provider, byWire) == sealNone {
			c.Selection = "not_consulted"
			c.Reason += " This tier is bypassed for this wire in the current launch."
			_, heldDefault := x.api[k.Provider]
			_, heldRoute := x.pinnedAPI[k.Provider]
			switch {
			case !active:
			case !x.policy.sealableSlot(slotOfProvider(k.Provider)):
				c.Reason += x.exclusionReason(slotOfProvider(k.Provider))
			case byWire && (heldDefault || heldRoute):
				c.Reason += " An earlier credential already funds " + string(k.Provider) + "."
			case isFacadeProvider(k.Provider) && !x.taken(string(k.Provider)):
				c.Reason += " A " + string(k.Provider) + " key is kept off the anthropic wire's default here (facade_default=" + string(x.policy.facade) + ")."
			}
		}
		i := x.add(c)
		if floorHeld {
			x.floorHeld[i] = true
		}
		// `first` feeds the restore stage, which hands a skipped candidate the
		// slot when nothing else filled the wire. A key excluded by its
		// audience must stay out of that too, or the preview would predict
		// exactly the hand-out the live resolver refuses.
		if !outOfAudience {
			first[k.Provider] = append(first[k.Provider], i)
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
		} else if candidates, ok := first[provider]; ok && outcome != sealNone {
			// Remembered for the restore only when the live fill would have
			// tried it: a key the policy withholds is not a refused one.
			x.skippedAPI[provider] = append(x.skippedAPI[provider], candidates...)
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
		x.conditional(tier + " subscription metadata unavailable; selection is conditional.")
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
		closed, floorHeld := x.windows(x.ctx, &c, usageBackendForKind(rec.Kind), meter, rec.Fingerprint, true)
		// The launch's gates bound this tier like every other: a forfait
		// whose provider the allowlist excludes is sealed by no tier, so the
		// candidate says so instead of predicting a credential the live
		// launch refuses to acquire.
		gated := !x.policy.sealableSlot(slotOfKind(kind))
		if !active || gated || byWire && x.taken(kind) {
			c.Selection = "not_consulted"
			c.Reason += " This tier is bypassed for this wire in the current launch."
			if gated {
				c.Reason += x.exclusionReason(slotOfKind(kind))
			}
		}
		i := x.add(c)
		if floorHeld {
			x.floorHeld[i] = true
		}
		if !active || gated {
			continue
		}
		if _, ok := x.oauth[kind]; ok {
			continue
		}
		if byWire && x.taken(kind) {
			continue
		}
		if closed {
			x.skippedOAuth[kind] = append(x.skippedOAuth[kind], i)
			continue
		}
		x.oauth[kind] = i
	}
}

// windows uses the same cached refusal/cap evaluation and probe predicates as
// launch. A needed probe is reported, never executed by an observation.
// floorHeld reports a credential a budget-floor reservation passes over for
// this bot — refused, yet not one the restore may hand back.
func (x *credentialPreview) windows(ctx context.Context, c *runview.CredentialPreviewCandidate, backend, meter, fp string, oauth bool) (closed, floorHeld bool) {
	if x.p.usageCaps == nil || backend == "" || fp == "" {
		c.Reason = "No usage-window observation is available; the launch assumes availability."
		return false, false
	}
	// The RAW bot id, the budget floor's key (previewBotID is the audience's
	// folded spelling): the launch judges these readings against the cap the
	// OTHER workloads' reservations leave this bot.
	botID := x.out.Context.BotID
	block := func(until time.Time, why string) {
		c.State = "blocked"
		c.Reason = why
		c.ReopensAt = &until
	}
	lctx, cancel := context.WithTimeout(ctx, usageCapLookupTimeout)
	defer cancel()
	key := usagecap.Key(backend, meter, fp)
	readings, err := x.p.usageCaps.Latest(lctx, key)
	if err != nil {
		// The launch asks the reservation even after a failed read: a bot
		// held out of the whole window needs no reading to be refused.
		if until, why := x.p.heldOutUntil(ctx, botID, x.out.ObservedAt); !until.IsZero() {
			block(until, why)
			return true, true
		}
		c.Reason = "Usage-window observation unavailable; the launch fails open."
		return false, false
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
	// refusedUntil's order: the credential's own evidence first, then the
	// reservation that holds the whole window back from this bot.
	until, why, held := x.p.evaluateCredentialWindows(ctx, readings, x.out.ObservedAt, botID)
	if until.IsZero() {
		until, why = x.p.heldOutUntil(ctx, botID, x.out.ObservedAt)
		held = !until.IsZero()
	}
	if !until.IsZero() {
		block(until, why)
		return true, held
	}
	// The same decision the launch makes, read through one predicate: a
	// preview that announced a probe the launch then skipped would be
	// telling the operator about work that never happens.
	if oauth && x.p.usageProbe != nil && (x.p.initialProbeWanted(key, fp, readings, x.out.ObservedAt) || x.p.staleReadingsSuggestClosed(ctx, readings, x.out.ObservedAt, botID)) {
		c.State = "probe_required"
		c.Reason = "Launch will probe this new or stale account; its availability is unknown until then."
		return false, false
	}
	if fresh {
		c.State = "available"
		c.Reason = "No fresh window refusal or operator cap blocks this credential."
	} else {
		c.State = "unknown"
		c.Reason = "No fresh usage-window observation; the launch assumes availability."
	}
	return false, false
}

// previewLaunchRepo names the repository a preview was asked for. A preview
// without one is not a launch that names none: the source may serve several
// repositories or a pattern, and the next delivery will name one the preview
// cannot know.
func previewLaunchRepo(repo string) string {
	if repo == "" {
		return "a repository (none is known to this preview: the source names several, a pattern, or none)"
	}
	return "repository " + repo
}
