package cloudpublisher

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// resolveWithRouting is resolvePolicy with a RESOLVED routing policy, and it
// hands back the workflow the resolution composed: the policy ladder lands
// its rungs on that program in place, so a test can read the walk the
// refusals read rather than the one it compiled.
func resolveWithRouting(t *testing.T, p *Publisher, src string, policy *store.RunLLMRoutePolicy) (*ir.Workflow, credResolution, error) {
	t.Helper()
	wf := compileTestSource(t, src)
	ctx := store.WithTenant(context.Background(), "team1")
	creds, err := p.resolveAndSealCredentials(ctx, "run-ladder", "", "team1", "owner1", "", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, policy, nil, boundaryDerived)
	return wf, creds, err
}

// forfaitLadderPolicy is the shape of Jo's 2026-10-17 comment: the claude
// forfait as a routing-policy rung, armed on the usage window. The key slots
// of both pinned routes are in the pair order too, so the policy's own slot
// whitelist is never what decides a provider is unfunded here — only the
// credentials the team holds are.
func forfaitLadderPolicy() *store.RunLLMRoutePolicy {
	return &store.RunLLMRoutePolicy{
		PairOrder: []string{
			llmroute.Pair(llmroute.HarnessClaw, "anthropic_key"),
			llmroute.Pair(llmroute.HarnessClaw, "openai_key"),
			llmroute.Pair(llmroute.HarnessClaudeCode, llmroute.CredClaudeForfait),
		},
		Triggers: []string{llmroute.TriggerUsageWindow},
	}
}

// zaiLadderPolicy is a rung the policy HOLDS through a provider key, so the
// rung carries that provider's hint — a route of its own, with a provider
// name, landed on the program by the ladder.
func zaiLadderPolicy() *store.RunLLMRoutePolicy {
	return &store.RunLLMRoutePolicy{
		PairOrder: []string{llmroute.Pair(llmroute.HarnessClaw, "zai_key")},
		Triggers:  []string{llmroute.TriggerUsageWindow},
	}
}

// #2485, the third disarm shape of Jo's 2026-10-17 comment and the one no
// witness held: a ROUTING POLICY's rungs, folded onto the program by
// ir.ApplyPolicyLadder before the refusals read their set. Both arms read
// that set ONCE and AFTER the ladder (publisher.go), and both halves of the
// ordering are judged here — a rung must not disarm the sovereign refusal,
// and a rung the run may actually spend must count for the knob's rule.
//
// No other test combines the sovereign policy with a routing policy, so the
// composition itself is what these two witnesses fence.
func TestResolve_LLMFallbackNone_APolicyLadderRungDoesNotDisarmTheRefusal(t *testing.T) {
	t.Run("a forfait rung does not disarm the sovereign refusal", func(t *testing.T) {
		p, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})
		// The team's OWN forfait: it funds anthropic
		// (providerOfOAuthKind) and it is what makes the policy's pair HELD,
		// so the ladder has a rung to land.
		seedOAuth(t, p.oauthForfait, p.sealer, "owner1", "sk-forfait-team")

		wf, creds, err := resolveWithRouting(t, p, mixedAttributableBot, forfaitLadderPolicy())

		if len(creds.Ladder) == 0 {
			t.Fatalf("no rung landed: this test does not exercise the shape it names — the policy's %s pair must be held by the team's own forfait", llmroute.Pair(llmroute.HarnessClaudeCode, llmroute.CredClaudeForfait))
		}
		if got := creds.Ladder[0].Provider; got != llmroute.CredClaudeForfait {
			t.Fatalf("first rung = %q, want %q — the team holds the forfait alone, so the forfait pair is the only HELD one and its rung is the route this shape folds in", got, llmroute.CredClaudeForfait)
		}
		// The premise of the disarm: on the COMPOSED program the walk is not
		// narrow-safe, so the pre-2026-10-28 reading (the NARROWING one,
		// `spendableProviders`) answered "may spend anything" and refused
		// nothing. The attributed reading is a lower bound the ladder can
		// only grow, which is what makes the rung cost a name instead of the
		// gate.
		if walk := model.EffectiveProviders(model.RouteView{Workflow: wf}, walkProviderSet); walk.NarrowSafe {
			t.Fatalf("the composed program is narrow-safe (providers=%v): the narrowing reading this fences was not empty here, so the test no longer covers the disarm it names", walk.Providers)
		}

		var unfunded *UnfundedProvidersError
		if !errors.As(err, &unfunded) {
			t.Fatalf("resolveAndSealCredentials = %v: the launch STARTED — the routing policy's rung emptied the set the refusal reads, so the pinned `openai` route the team funds nothing of spends the runner pod's env on tiers=[oauth-forfait]", err)
		}
		if got := unfunded.Providers; len(got) != 1 || got[0] != "openai" {
			t.Fatalf("unfunded providers = %v, want exactly [openai]: the team's own forfait funds `anthropic`, and neither `free` nor the rung names a provider of its own", got)
		}
		if sites := unfunded.Sites["openai"]; len(sites) != 1 || sites[0] != "verify" {
			t.Errorf("sites naming openai = %v, want [verify] — the refusal must name the node that spends it", sites)
		}
		if !unfunded.Sovereign {
			t.Error("the refusal does not say it came from the sovereign posture — the operator's remedy differs from the knob's")
		}

		// The control, same ladder: a team funding every provider its routes
		// name starts, so the refusal above is the route's and not the rung's.
		q, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})
		seedOAuth(t, q.oauthForfait, q.sealer, "owner1", "sk-forfait-team")
		seedKeyFP(t, q.apiKeys, q.sealer, "team1", secrets.ProviderOpenAI, "sk-openai-team", "fp-openai-team")
		if _, _, err := resolveWithRouting(t, q, mixedAttributableBot, forfaitLadderPolicy()); err != nil {
			t.Fatalf("resolveAndSealCredentials = %v, want nil — the team's own credentials fund every provider its routes name", err)
		}
	})

	t.Run("the knob counts the set the ladder composed", func(t *testing.T) {
		p, _ := llmFallbackPublisher(t, nil)
		p.requireLLMCredential = true
		// The team holds the zai key the policy's pair is HELD by — so the
		// rung is a route this run may really spend when the window closes.
		seedKeyFP(t, p.apiKeys, p.sealer, "team1", secrets.ProviderZAI, "sk-zai-team", "fp-zai-team")
		funded := fundedProviderSet([]secrets.Provider{secrets.ProviderZAI}, nil, unfundedPosture{})

		compiled := model.EffectiveProviders(model.RouteView{Workflow: compileTestSource(t, twoRoutesBot)}, walkProviderSet)
		wf, creds, err := resolveWithRouting(t, p, twoRoutesBot, zaiLadderPolicy())
		composed := model.EffectiveProviders(model.RouteView{Workflow: wf}, walkProviderSet)

		if len(creds.Ladder) == 0 {
			t.Fatalf("no rung landed: the policy's %s pair must be held by the team's zai key", llmroute.Pair(llmroute.HarnessClaw, "zai_key"))
		}
		// The ordering, as a fact about the set: the rung's provider is named
		// only on the COMPOSED program, at its own site.
		if sites := composed.Pins["zai"]; len(sites) != 1 || sites[0] != "draft#run-fallback" {
			t.Fatalf("composed pins for zai = %v, want [draft#run-fallback] — the rung must be a route of the set the refusals judge", sites)
		}
		if _, named := compiled.Pins["zai"]; named {
			t.Fatalf("the COMPILED program already names zai (%v): this test cannot see the ordering it judges", compiled.Pins)
		}
		// The premise, stated through the producer the launch reads: on the
		// pre-ladder set EVERY attributed provider is unfunded, which is
		// exactly what the knob refuses.
		if r := unfundedRefusalFor("team1", routeAttribution{providers: compiled.Providers, pins: compiled.Pins}, funded, false, unfundedPosture{Knob: true}, nil); r == nil {
			t.Fatalf("premise lost: the pre-ladder set %v no longer refuses under the knob, so this test cannot see the ordering it names", compiled.Providers)
		}
		if err != nil {
			t.Fatalf("resolveAndSealCredentials = %v, want nil — the set was read BEFORE the ladder, so the policy's rung (a route this run may spend, funded by the team's zai key) did not count and the knob refused a run that has funded routing left", err)
		}
	})
}

// previewWarning returns the warning carrying s, and "" when none does.
func previewWarning(v runview.CredentialPreview, s string) string {
	for _, w := range v.Warnings {
		if strings.Contains(w, s) {
			return w
		}
	}
	return ""
}

// The preview MIRRORS the unfunded refusal by name (#2485): it answers the
// launch's own sentence, built by the launch's own producer
// (unfundedRefusalFor) on the launch's own arming (unfundedPostureFor), so an
// operator who previews a workflow reads the verdict the launch will give it
// — provider AND node — instead of a generic "a route nothing of the team's
// own funds refuses the launch" beside candidates that look like a run.
//
// The preview is run BEFORE the launch, the order an operator works in.
func TestCredentialPreview_AnswersTheLaunchsUnfundedSentence_LLMFallbackNone(t *testing.T) {
	t.Run("an unfunded pinned provider: the same sentence as the launch", func(t *testing.T) {
		p, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})
		seedKeyFP(t, p.apiKeys, p.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team", "fp-ant-team")

		v, err := p.PreviewCredentials(context.Background(), previewSpec("team1", "owner1"), compileTestSource(t, mixedAttributableBot))
		if err != nil {
			t.Fatalf("PreviewCredentials: %v", err)
		}
		launchErr := resolvePolicy(t, p, mixedAttributableBot)
		var unfunded *UnfundedProvidersError
		if !errors.As(launchErr, &unfunded) {
			t.Fatalf("resolveAndSealCredentials = %v, want an *UnfundedProvidersError naming openai — the premise of this parity test", launchErr)
		}

		want := "The live launch refuses: " + launchErr.Error()
		if got := previewWarning(v, want); got == "" {
			t.Fatalf("the preview does not answer the launch's own sentence.\n want: %s\n got warnings: %v", want, v.Warnings)
		}
		// The two facts an operator acts on, asserted on the preview itself:
		// which provider to provision, and which node spends it.
		for _, fact := range []string{"openai", "verify"} {
			if previewWarning(v, fact) == "" {
				t.Errorf("no preview warning names %q: %v", fact, v.Warnings)
			}
		}
		if w := previewWarning(v, "anthropic (named by"); w != "" {
			t.Errorf("the preview names a provider the launch funds: %q", w)
		}
	})

	t.Run("the team's own forfait funds the route: both start", func(t *testing.T) {
		p, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})
		seedOAuth(t, p.oauthForfait, p.sealer, "owner1", "sk-forfait-team")

		v, err := p.PreviewCredentials(context.Background(), previewSpec("team1", "owner1"), compileTestSource(t, anthropicPinnedBot))
		if err != nil {
			t.Fatalf("PreviewCredentials: %v", err)
		}
		if err := resolvePolicy(t, p, anthropicPinnedBot); err != nil {
			t.Fatalf("resolveAndSealCredentials = %v, want nil — the team's own forfait funds the anthropic route", err)
		}
		// One funding reading for both surfaces (fundedProviderSet): a
		// preview blind to the OAuth channel would refuse a launch that
		// starts, which is the same defect as the silence it replaces.
		if w := previewWarning(v, runview.ErrNoLLMCredential.Error()); w != "" {
			t.Errorf("the preview refuses a launch that starts: %q", w)
		}
	})
}

// The knob's arm (ITERION_CLOUD_REQUIRE_LLM_CREDENTIAL) is mirrored with ITS
// rule, not the sovereign one: it refuses only a run whose EVERY attributed
// provider is unfunded, so a preview applying the strict reading would warn
// where the launch starts. The knob half of the gap predates #2485 — the
// preview never mirrored this refusal at all, and said "runner environment
// availability is unknown" about a run the publisher would not publish.
func TestCredentialPreview_AnswersTheLaunchsUnfundedSentence_RequireLLMCredential(t *testing.T) {
	t.Run("nothing funded: the same sentence as the launch", func(t *testing.T) {
		p, _ := llmFallbackPublisher(t, nil)
		p.requireLLMCredential = true

		v, err := p.PreviewCredentials(context.Background(), previewSpec("team1", "owner1"), compileTestSource(t, mixedAttributableBot))
		if err != nil {
			t.Fatalf("PreviewCredentials: %v", err)
		}
		launchErr := resolvePolicy(t, p, mixedAttributableBot)
		var unfunded *UnfundedProvidersError
		if !errors.As(launchErr, &unfunded) {
			t.Fatalf("resolveAndSealCredentials = %v, want the knob's *UnfundedProvidersError — the premise of this parity test", launchErr)
		}
		if unfunded.Sovereign {
			t.Fatalf("the launch's refusal claims the sovereign posture: %v", launchErr)
		}

		want := "The live launch refuses: " + launchErr.Error()
		if got := previewWarning(v, want); got == "" {
			t.Fatalf("the preview does not answer the knob's own sentence.\n want: %s\n got warnings: %v", want, v.Warnings)
		}
	})

	t.Run("one funded provider: silent where the launch starts", func(t *testing.T) {
		p, _ := llmFallbackPublisher(t, nil)
		p.requireLLMCredential = true
		seedKeyFP(t, p.apiKeys, p.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team", "fp-ant-team")

		v, err := p.PreviewCredentials(context.Background(), previewSpec("team1", "owner1"), compileTestSource(t, mixedAttributableBot))
		if err != nil {
			t.Fatalf("PreviewCredentials: %v", err)
		}
		if err := resolvePolicy(t, p, mixedAttributableBot); err != nil {
			t.Fatalf("resolveAndSealCredentials = %v, want nil — the knob's compromise is deliberate: one funded provider lets the run start", err)
		}
		if w := previewWarning(v, runview.ErrNoLLMCredential.Error()); w != "" {
			t.Errorf("the preview refuses a launch that starts: %q — the knob's arm must keep the knob's rule, not the sovereign one", w)
		}
	})
}

// The preview judges the program the LAUNCH judges (#2485): the launch reads
// its attributed set AFTER ir.ApplyPolicyLadder landed the routing policy's
// held rungs, and a rung's own provider is a route the run may really spend —
// which is exactly what the knob's permissive rule counts. A preview reading
// the program as AUTHORED counts one provider fewer, so its arm flips to
// "every attributed provider is unfunded" and it prints the launch's refusal
// for a run the launch starts: the operator is told to provision two
// credentials and to unset a server env var for a run that would have run.
//
// Both surfaces therefore compose through ONE producer (composePolicyLadder),
// and the preview RESTORES what it composed (snapshotFallbackRoutes) — the
// program it was handed must come back as it was, because the fallback-door
// block after the mirror answers on the authored program exactly as the live
// door does (it runs before the ladder).
func TestCredentialPreview_JudgesTheProgramThePolicyLadderComposed(t *testing.T) {
	p, _ := llmFallbackPublisher(t, nil)
	p.requireLLMCredential = true
	// The team holds ONLY the zai key the policy's pair is HELD by, and every
	// AUTHORED route of the workflow names a provider it funds nothing of.
	seedKeyFP(t, p.apiKeys, p.sealer, "team1", secrets.ProviderZAI, "sk-zai-team", "fp-zai-team")

	previewWf := compileTestSource(t, twoRoutesBot)
	authored := model.EffectiveProviders(model.RouteView{Workflow: previewWf}, walkProviderSet)
	// The premise, stated through the producer both surfaces read: on the
	// AUTHORED program the knob's rule refuses. Without it this witness
	// cannot see the stage it judges.
	if r := unfundedRefusalFor("team1", routeAttribution{providers: authored.Providers, pins: authored.Pins},
		fundedProviderSet([]secrets.Provider{secrets.ProviderZAI}, nil, unfundedPosture{}), false, unfundedPosture{Knob: true}, nil); r == nil {
		t.Fatalf("premise lost: the authored set %v no longer refuses under the knob", authored.Providers)
	}

	spec := previewSpec("team1", "owner1")
	spec.LLMRoutePolicy = zaiLadderPolicy()
	v, err := p.PreviewCredentials(context.Background(), spec, previewWf)
	if err != nil {
		t.Fatalf("PreviewCredentials: %v", err)
	}

	// The launch, a FRESH publisher seeded identically: it starts.
	q, _ := llmFallbackPublisher(t, nil)
	q.requireLLMCredential = true
	seedKeyFP(t, q.apiKeys, q.sealer, "team1", secrets.ProviderZAI, "sk-zai-team", "fp-zai-team")
	launchWf, creds, launchErr := resolveWithRouting(t, q, twoRoutesBot, zaiLadderPolicy())
	if len(creds.Ladder) == 0 {
		t.Fatalf("no rung landed: this witness does not exercise the shape it names — the policy's %s pair must be held by the team's zai key", llmroute.Pair(llmroute.HarnessClaw, "zai_key"))
	}
	if launchErr != nil {
		t.Fatalf("resolveAndSealCredentials = %v, want nil — the ladder's rung is funded routing this run may spend", launchErr)
	}
	if sites := model.EffectiveProviders(model.RouteView{Workflow: launchWf}, walkProviderSet).Pins["zai"]; len(sites) != 1 || sites[0] != "draft#run-fallback" {
		t.Fatalf("the launch's composed pins for zai = %v, want [draft#run-fallback]: the program this witness compares against is not the composed one", sites)
	}

	if w := previewWarning(v, runview.ErrNoLLMCredential.Error()); w != "" {
		t.Errorf("the preview refuses a launch that starts: %q — it read the program as AUTHORED, where the launch reads the one its routing policy composed", w)
	}
	// Composing is a READING here: the rung must not survive the mirror.
	if after := model.EffectiveProviders(model.RouteView{Workflow: previewWf}, walkProviderSet); len(after.Pins["zai"]) != 0 {
		t.Errorf("the preview left the ladder's rung on the program it was handed (pins=%v): a read model restores what it composed", after.Pins)
	}
}

// The THIRD arm of the same refusal, mirrored like the two others (#2485):
// under `llm_fallback: none`, a workflow NO route of which the walk can
// attribute refuses when the team holds nothing at all — the pod's platform
// env would be the only funding left. `agent draft: backend: "claude_code"`
// is the most ordinary shape in the catalogue, and the preview used to answer
// it with "runner environment availability is unknown" about a run the
// publisher will not publish: the silence this change exists to close,
// surviving for the one arm that lived outside the producer.
func TestCredentialPreview_AnswersTheLaunchsUnattributableSentence_LLMFallbackNone(t *testing.T) {
	t.Run("nothing of the team's own, no route the walk can read: the same sentence", func(t *testing.T) {
		p, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})

		v, err := p.PreviewCredentials(context.Background(), previewSpec("team1", "owner1"), compileTestSource(t, unattributableBot))
		if err != nil {
			t.Fatalf("PreviewCredentials: %v", err)
		}
		launchErr := resolvePolicy(t, p, unattributableBot)
		var unfunded *UnfundedProvidersError
		if !errors.As(launchErr, &unfunded) {
			t.Fatalf("resolveAndSealCredentials = %v, want an *UnfundedProvidersError — this arm's facts must travel with the refusal, not inside its text", launchErr)
		}
		if !unfunded.Unattributable || !unfunded.Sovereign || len(unfunded.Providers) != 0 {
			t.Errorf("the refusal does not carry the arm that produced it: %+v", unfunded)
		}
		if !errors.Is(launchErr, runview.ErrNoLLMCredential) {
			t.Errorf("the refusal stopped unwrapping to runview.ErrNoLLMCredential: the 422 every reader answers with reads it")
		}

		want := "The live launch refuses: " + launchErr.Error()
		if got := previewWarning(v, want); got == "" {
			t.Fatalf("the preview does not answer the launch's own sentence.\n want: %s\n got warnings: %v", want, v.Warnings)
		}
	})

	t.Run("a credential of the team's own keeps the compromise: both start", func(t *testing.T) {
		p, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})
		seedKeyFP(t, p.apiKeys, p.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team", "fp-ant-team")

		v, err := p.PreviewCredentials(context.Background(), previewSpec("team1", "owner1"), compileTestSource(t, unattributableBot))
		if err != nil {
			t.Fatalf("PreviewCredentials: %v", err)
		}
		if err := resolvePolicy(t, p, unattributableBot); err != nil {
			t.Fatalf("resolveAndSealCredentials = %v, want nil — a route the walk cannot read is refused by no name", err)
		}
		if w := previewWarning(v, runview.ErrNoLLMCredential.Error()); w != "" {
			t.Errorf("the preview refuses a launch that starts: %q — the compromise the unattributable routes are owed is the launch's too", w)
		}
	})
}

// poolMappedPublisher is llmFallbackPublisher with the team mapped to a
// RUNNER POOL (D13) — the second posture that blocks the shared tiers, beside
// `llm_fallback: none`, and the one the preview had no notion of at all.
func poolMappedPublisher(t *testing.T, pool string, llmFallback map[string]string) *Publisher {
	t.Helper()
	p, _ := llmFallbackPublisher(t, llmFallback)
	p.identity = &fakeTeamResolver{orgs: map[string]string{"team1": ""}, llmFallback: llmFallback,
		runnerPool: map[string]string{"team1": pool}}
	return p
}

// resolveAsLaunched is resolvePolicy with the runner pool the LAUNCH would
// pass: SubmitLaunch resolves the mapping (runnerPoolForTeam) and hands it to
// resolveAndSealCredentials, so a witness that hard-coded the pool instead
// would be comparing two surfaces fed different inputs — the very thing the
// parity is about.
func resolveAsLaunched(t *testing.T, p *Publisher, src string) error {
	t.Helper()
	ctx := store.WithTenant(context.Background(), "team1")
	pool, err := p.runnerPoolForTeam(ctx, "team1")
	if err != nil {
		t.Fatalf("runnerPoolForTeam: %v", err)
	}
	wf := compileTestSource(t, src)
	_, rerr := p.resolveAndSealCredentials(ctx, "run-pooled", "", "team1", "owner1", "", pool, wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, nil, nil, boundaryDerived)
	return rerr
}

// A team mapped to a RUNNER POOL consults no shared tier either (D13,
// publisher.go: `runnerPool != "" || sovereign`), and the preview knew
// nothing of that mapping: it filled from the platform tier, selected those
// candidates, and computed the launch's verdict on fills the launch never
// acquires — silence for a launch refused by name, the exact defect #2485
// closes for the sovereign posture.
//
// The witness drives the PUBLIC surface of both sides, and the launch takes
// its pool from the same reader the preview now uses.
func TestCredentialPreview_AnswersTheLaunchsUnfundedSentence_PoolMappedTeam(t *testing.T) {
	// The platform tier funds BOTH providers the workflow pins, and the team
	// holds nothing: the only difference between the two runs below is the
	// pool mapping.
	seedPlatform := func(t *testing.T, p *Publisher) {
		t.Helper()
		seedKeyFP(t, p.apiKeys, p.sealer, secrets.PlatformTenantID, secrets.ProviderAnthropic, "sk-ant-platform", "fp-ant-platform")
		seedKeyFP(t, p.apiKeys, p.sealer, secrets.PlatformTenantID, secrets.ProviderOpenAI, "sk-openai-platform", "fp-openai-platform")
	}

	t.Run("mapped to a pool: the same sentence as the launch", func(t *testing.T) {
		p := poolMappedPublisher(t, "pool-a", nil)
		p.requireLLMCredential = true
		seedPlatform(t, p)

		v, err := p.PreviewCredentials(context.Background(), previewSpec("team1", "owner1"), compileTestSource(t, mixedAttributableBot))
		if err != nil {
			t.Fatalf("PreviewCredentials: %v", err)
		}
		launchErr := resolveAsLaunched(t, p, mixedAttributableBot)
		var unfunded *UnfundedProvidersError
		if !errors.As(launchErr, &unfunded) {
			t.Fatalf("resolveAndSealCredentials = %v, want an *UnfundedProvidersError — a pool team's bundle carries no platform key, so both pinned routes are unfunded", launchErr)
		}

		want := "The live launch refuses: " + launchErr.Error()
		if got := previewWarning(v, want); got == "" {
			t.Fatalf("the preview does not answer the launch's own sentence.\n want: %s\n got warnings: %v", want, v.Warnings)
		}
		// The fills the verdict is computed on: a pool-blind preview selected
		// the platform keys, which is how it came to answer "this run starts".
		for _, c := range v.Candidates {
			switch c.Tier {
			case "org", "pool", "platform":
				t.Errorf("candidate tier %q (%s) shown for a pool-mapped team — the launch seals no shared-tier credential into a pool pod's bundle", c.Tier, c.Provider)
			}
		}
		if previewWarning(v, `runner pool "pool-a"`) == "" {
			t.Errorf("no warning names the posture that holds the shared tiers out: %v", v.Warnings)
		}
		// The pool block's reason code names the same posture as the
		// warning, never the empty-bundle gate's "credentials_present".
		if v.Pool.Considered || v.Pool.Reason != "runner_pool" {
			t.Errorf("pool block = considered %v reason %q, want unconsidered with reason %q", v.Pool.Considered, v.Pool.Reason, "runner_pool")
		}
	})

	t.Run("the same team unmapped: both start on the platform tier", func(t *testing.T) {
		p, _ := llmFallbackPublisher(t, nil)
		p.requireLLMCredential = true
		seedPlatform(t, p)

		v, err := p.PreviewCredentials(context.Background(), previewSpec("team1", "owner1"), compileTestSource(t, mixedAttributableBot))
		if err != nil {
			t.Fatalf("PreviewCredentials: %v", err)
		}
		if err := resolveAsLaunched(t, p, mixedAttributableBot); err != nil {
			t.Fatalf("resolveAndSealCredentials = %v, want nil — an unmapped team's launch does consult the platform tier, so the refusal above is the MAPPING's and not the seeding's", err)
		}
		if w := previewWarning(v, runview.ErrNoLLMCredential.Error()); w != "" {
			t.Errorf("the preview refuses a launch that starts: %q", w)
		}
	})

	t.Run("mapped to a pool, funded by the team's own keys: both start", func(t *testing.T) {
		p := poolMappedPublisher(t, "pool-a", nil)
		p.requireLLMCredential = true
		seedKeyFP(t, p.apiKeys, p.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team", "fp-ant-team")
		seedKeyFP(t, p.apiKeys, p.sealer, "team1", secrets.ProviderOpenAI, "sk-openai-team", "fp-openai-team")

		v, err := p.PreviewCredentials(context.Background(), previewSpec("team1", "owner1"), compileTestSource(t, mixedAttributableBot))
		if err != nil {
			t.Fatalf("PreviewCredentials: %v", err)
		}
		if err := resolveAsLaunched(t, p, mixedAttributableBot); err != nil {
			t.Fatalf("resolveAndSealCredentials = %v, want nil — the team's own keys fund every provider its routes name", err)
		}
		if w := previewWarning(v, runview.ErrNoLLMCredential.Error()); w != "" {
			t.Errorf("the preview refuses a launch that starts: %q — a pool mapping takes the shared tiers away, not the team's own credentials", w)
		}
	})
}

// The preview HOLDS a policy pair only where its own metadata serves the
// slot (doorServesMetadata, doorServes' twin): a rung the run cannot occupy
// is no route of the run, and composing it anyway lands a provider name at
// `node#run-fallback` that the refusal then reads back. The failure
// direction is the worst one — a FALSE REFUSAL: the operator is told to
// provision a provider for a launch that starts, named by a rung the run
// never gets.
//
// No other witness can see this predicate: the one that carries a routing
// policy holds the single pair the team already holds, so a predicate that
// said yes to everything would change nothing there.
func TestCredentialPreview_HoldsOnlyThePairsItsOwnMetadataServes(t *testing.T) {
	p, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})
	// The team holds the policy's anthropic_key pair and nothing else: its
	// openai_key and claude-forfait pairs are LISTED and unheld.
	seedKeyFP(t, p.apiKeys, p.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team", "fp-ant-team")

	// The premise, through the composer and the producer both surfaces read:
	// a surface holding EVERY listed pair lands rungs whose providers the
	// team funds nothing of, and the sovereign posture refuses them by name.
	probe := compileTestSource(t, anthropicPinnedBot)
	probeView := model.RouteView{Workflow: probe}
	composePolicyLadder(probe, probeView, forfaitLadderPolicy(), nil, nil, func(_, _ string) bool { return true }, false, nil)
	probeRoutes := attributedRoutes(probeView)
	if r := unfundedRefusalFor("team1", probeRoutes,
		fundedProviderSet([]secrets.Provider{secrets.ProviderAnthropic}, nil, unfundedPosture{}), false, unfundedPosture{Sovereign: true}, nil); r == nil {
		t.Fatalf("premise lost: holding every pair the policy lists no longer names an unfunded provider (attributed=%v) — this witness cannot see the held-pair predicate", probeRoutes.providers)
	}

	spec := previewSpec("team1", "owner1")
	spec.LLMRoutePolicy = forfaitLadderPolicy()
	v, err := p.PreviewCredentials(context.Background(), spec, compileTestSource(t, anthropicPinnedBot))
	if err != nil {
		t.Fatalf("PreviewCredentials: %v", err)
	}

	// The launch, a fresh publisher seeded identically: it starts — the
	// policy's unheld pairs are no routes of this run.
	q, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})
	seedKeyFP(t, q.apiKeys, q.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team", "fp-ant-team")
	if _, _, err := resolveWithRouting(t, q, anthropicPinnedBot, forfaitLadderPolicy()); err != nil {
		t.Fatalf("resolveAndSealCredentials = %v, want nil — the team's own key funds the one route this workflow pins, and an unheld pair composes no rung", err)
	}
	if w := previewWarning(v, runview.ErrNoLLMCredential.Error()); w != "" {
		t.Errorf("the preview refuses a launch that starts: %q — it held a pair its own metadata does not serve, so the rung it composed named a provider this run never routes to", w)
	}
}

// metadataFunding is the launch's funding reading on the fills a preview
// PREDICTS, and the twinning is the whole point: the same three channels,
// one per source. A twin blind to a channel refuses a launch that starts —
// and the pinned-key channel is the case fundedAPIKeySlots calls "exactly
// the case these gates must not refuse".
//
// Judged against bundleFunding on the equivalent SEALED bundle, so the
// assertion is the twinning itself and not a second expectation to keep up
// to date. One provider per channel: each channel is load-bearing alone.
func TestMetadataFunding_TwinsTheBundleReadingChannelForChannel(t *testing.T) {
	x := &credentialPreview{
		api:       map[secrets.Provider]int{secrets.ProviderZAI: 0},
		pinnedAPI: map[secrets.Provider]int{secrets.ProviderOpenAI: 1},
		oauth:     map[string]int{string(secrets.OAuthKindClaudeCode): 2},
	}
	bundle := secrets.RunBundle{
		APIKeys:          map[secrets.Provider]string{secrets.ProviderZAI: "sk-zai"},
		PinnedAPIKeys:    map[secrets.Provider]string{secrets.ProviderOpenAI: "sk-openai"},
		OAuthCredentials: map[string][]byte{string(secrets.OAuthKindClaudeCode): []byte("blob")},
	}

	got := metadataFunding(x, unfundedPosture{})
	if want := bundleFunding(bundle, unfundedPosture{}); !reflect.DeepEqual(got, want) {
		t.Fatalf("metadataFunding = %v, bundleFunding of the equivalent bundle = %v — the two sources answer one reading or the preview's verdict stops being the launch's", got, want)
	}
	// Named, so a reading that lost a channel on BOTH sides at once is still
	// caught: zai through the default slots, openai through the PINNED ones,
	// anthropic through the forfait's provider.
	for _, prov := range []string{"zai", "openai", "anthropic"} {
		if !got[prov] {
			t.Errorf("funded[%q] = false: the channel that funds it is not read — a route naming it would be refused on a run that holds its credential", prov)
		}
	}
	if len(got) != 3 {
		t.Errorf("funded = %v, want exactly the three providers the channels name", got)
	}
}

// A refused launch publishes nothing, so the runner-environment question is
// one the launch never asks: the mirror REPLACES "runner environment
// availability is unknown", it does not sit beside it — an operator reading
// "unknown" and "refuses" about one launch is reading two verdicts. The line
// stays exactly where the env question is real: nothing predicted, no
// refusal armed, the runner may fund the run from its own environment.
func TestCredentialPreview_TheRefusalRetiresTheRunnerEnvironmentLine(t *testing.T) {
	const envLine = "No database credential is predicted; runner environment availability is unknown."

	t.Run("the launch refuses: no environment line beside the verdict", func(t *testing.T) {
		p, _ := llmFallbackPublisher(t, nil)
		p.requireLLMCredential = true

		v, err := p.PreviewCredentials(context.Background(), previewSpec("team1", "owner1"), compileTestSource(t, mixedAttributableBot))
		if err != nil {
			t.Fatalf("PreviewCredentials: %v", err)
		}
		if previewWarning(v, "The live launch refuses") == "" {
			t.Fatalf("premise lost: the knob's refusal is not mirrored, so this witness cannot see the two lines it fences: %v", v.Warnings)
		}
		if len(v.Wires) != 0 {
			t.Fatalf("premise lost: wires %v predicted for a team holding nothing", v.Wires)
		}
		if w := previewWarning(v, envLine); w != "" {
			t.Errorf("the environment line sits beside the refusal: %q — the publisher never publishes this run, so its runner's environment is moot", w)
		}
	})

	t.Run("nothing predicted, nothing armed: the environment line stays", func(t *testing.T) {
		p, _ := llmFallbackPublisher(t, nil)

		v, err := p.PreviewCredentials(context.Background(), previewSpec("team1", "owner1"), compileTestSource(t, mixedAttributableBot))
		if err != nil {
			t.Fatalf("PreviewCredentials: %v", err)
		}
		if err := resolvePolicy(t, p, mixedAttributableBot); err != nil {
			t.Fatalf("resolveAndSealCredentials = %v, want nil — with no posture armed the runner's env may fund this run, which is what the line is about", err)
		}
		if w := previewWarning(v, "The live launch refuses"); w != "" {
			t.Fatalf("premise lost: a refusal is mirrored where the launch starts: %q", w)
		}
		if previewWarning(v, envLine) == "" {
			t.Errorf("the environment line is gone where the env question is real: %v", v.Warnings)
		}
	})
}

// erroringOAuthStore fails every ListByUser: the forfait channel's metadata
// is unavailable at preview time while the records themselves are intact.
type erroringOAuthStore struct{ secrets.OAuthStore }

func (e erroringOAuthStore) ListByUser(context.Context, string) ([]secrets.OAuthRecord, error) {
	return nil, errors.New("mongo down")
}

// The mirror's verdict is a FACT only on fills every tier answered for: a
// tier whose metadata could not be read ("selection is conditional") may
// hold the very credential that funds a route named here, so the fills are
// under-predicted and a definitive "The live launch refuses" on them is a
// false refusal — the operator is told to provision a credential for a run
// that starts. Every degraded tier read goes through ONE setter
// (credentialPreview.conditional), and the mirror then says what it could
// not see. Witnessed on two channels under the two postures: the platform
// API-key tier under the knob, the team's forfait under llm_fallback=none —
// the sovereign arm is conditional too, its own OAuth tier is a read.
//
// Both launches run on a fresh publisher whose store READS: they start, so
// the preview's refusal is the degradation's and not the seeding's.
func TestCredentialPreview_AFailedTierReadQualifiesTheMirror(t *testing.T) {
	const qualified = "The live launch refuses unless a tier whose metadata was unavailable funds the routes named here: "

	t.Run("the platform API-key read fails under the knob", func(t *testing.T) {
		q, _ := llmFallbackPublisher(t, nil)
		q.requireLLMCredential = true
		seedKeyFP(t, q.apiKeys, q.sealer, secrets.PlatformTenantID, secrets.ProviderAnthropic, "sk-ant-platform", "fp-ant-platform")
		seedKeyFP(t, q.apiKeys, q.sealer, secrets.PlatformTenantID, secrets.ProviderOpenAI, "sk-openai-platform", "fp-openai-platform")
		if err := resolvePolicy(t, q, mixedAttributableBot); err != nil {
			t.Fatalf("resolveAndSealCredentials = %v, want nil — the platform tier funds both pinned providers", err)
		}

		p, _ := llmFallbackPublisher(t, nil)
		p.requireLLMCredential = true
		seedKeyFP(t, p.apiKeys, p.sealer, secrets.PlatformTenantID, secrets.ProviderAnthropic, "sk-ant-platform", "fp-ant-platform")
		seedKeyFP(t, p.apiKeys, p.sealer, secrets.PlatformTenantID, secrets.ProviderOpenAI, "sk-openai-platform", "fp-openai-platform")
		p.apiKeys = erroringApiKeyStore{p.apiKeys}

		v, err := p.PreviewCredentials(context.Background(), previewSpec("team1", "owner1"), compileTestSource(t, mixedAttributableBot))
		if err != nil {
			t.Fatalf("PreviewCredentials: %v", err)
		}
		if previewWarning(v, "Platform API-key metadata unavailable; selection is conditional.") == "" {
			t.Fatalf("premise lost: the platform tier's read did not degrade, so this witness cannot see the fills it names: %v", v.Warnings)
		}
		if w := previewWarning(v, "The live launch refuses: "); w != "" {
			t.Errorf("a definitive verdict on under-predicted fills: %q — the launch starts on the platform keys the preview could not read", w)
		}
		got := previewWarning(v, qualified)
		if got == "" {
			t.Fatalf("the mirror does not say what it could not see.\n want prefix: %s\n got warnings: %v", qualified, v.Warnings)
		}
		// Still the producer's own sentence after the qualifier: the facts an
		// operator acts on if the tier really holds nothing.
		for _, fact := range []string{"anthropic (named by draft)", "openai (named by verify)"} {
			if !strings.Contains(got, fact) {
				t.Errorf("the qualified mirror lost the launch's own sentence (%q missing): %q", fact, got)
			}
		}
	})

	t.Run("the team's forfait read fails under llm_fallback=none", func(t *testing.T) {
		q, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})
		seedOAuth(t, q.oauthForfait, q.sealer, "owner1", "sk-forfait-team")
		if err := resolvePolicy(t, q, anthropicPinnedBot); err != nil {
			t.Fatalf("resolveAndSealCredentials = %v, want nil — the team's own forfait funds the anthropic route", err)
		}

		p, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})
		seedOAuth(t, p.oauthForfait, p.sealer, "owner1", "sk-forfait-team")
		p.oauthForfait = erroringOAuthStore{p.oauthForfait}

		v, err := p.PreviewCredentials(context.Background(), previewSpec("team1", "owner1"), compileTestSource(t, anthropicPinnedBot))
		if err != nil {
			t.Fatalf("PreviewCredentials: %v", err)
		}
		if previewWarning(v, "user subscription metadata unavailable; selection is conditional.") == "" {
			t.Fatalf("premise lost: the forfait tier's read did not degrade: %v", v.Warnings)
		}
		if w := previewWarning(v, "The live launch refuses: "); w != "" {
			t.Errorf("a definitive verdict on under-predicted fills: %q — the launch starts on the forfait the preview could not read", w)
		}
		got := previewWarning(v, qualified)
		if got == "" {
			t.Fatalf("the mirror does not say what it could not see.\n want prefix: %s\n got warnings: %v", qualified, v.Warnings)
		}
		if !strings.Contains(got, "anthropic (named by draft)") {
			t.Errorf("the qualified mirror lost the launch's own sentence: %q", got)
		}
	})
}
