package cloudpublisher

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// hintPinnedClaudeCodeBot is a claude_code route on a bare model whose
// provider is pinned by its hint: the walk names anthropic for it.
const hintPinnedClaudeCodeBot = "agent draft:\n  model: \"claude-opus-5\"\n  provider: \"anthropic\"\n  backend: \"claude_code\"\n\nworkflow main:\n  draft -> done\n"

// openAIClawBot is a claw route on a provider prefix: the walk names openai.
const openAIClawBot = "agent draft:\n  model: \"openai/gpt-5.4-mini\"\n  backend: \"claw\"\n\nworkflow main:\n  draft -> done\n"

// resolvedDefaultPolicy is what every production launch carries when no
// level set a routing policy: the fold's normalized default, its pair order
// headed by the two forfait pairs and every trigger armed.
func resolvedDefaultPolicy() *store.RunLLMRoutePolicy {
	pol := llmroute.Normalize(llmroute.Policy{})
	return &store.RunLLMRoutePolicy{PairOrder: pol.PairOrder, Triggers: pol.Triggers, RefusedPinnedKey: pol.RefusedPinnedKey, CrossHarness: pol.CrossHarness}
}

// A forfait funds no fork run (it cannot cross the run's sandbox, ADR-162
// decision 7), so under the untrusted posture a forfait pair is not HELD: the
// routing policy lands no forfait rung on a fork run's program. Such a rung
// carries no provider hint (a forfait slot has none) and, on claude_code, a
// bare model — a route the walk cannot attribute, which the untrusted arm
// refuses with a remedy no override reaches. Every row's team holds the key
// its route names AND a forfait, under the resolved default policy the
// production launch carries; each is judged on the gate's judge
// (FundingRefusal) and at publish (resolveAndSealCredentials), with the
// trusted control on the same stores.
//
// Mutations, each seen red and restored byte-identical:
//   - doorServesMetadata no longer reading the posture → the Claude row's
//     judge refuses the fork run on the forfait rung's site
//     (`draft#run-fallback`, unattributed);
//   - doorServes no longer reading the posture → the Claude row is refused
//     at publish on the same site, and the ChatGPT row's ladder carries the
//     codex forfait stage.
func TestFundingRefusal_AForkRunLandsNoForfaitRung(t *testing.T) {
	for _, row := range []struct {
		name string
		bot  string
		seed func(t *testing.T, p *Publisher)
	}{
		{
			name: "a Claude forfait beside the team's Anthropic key",
			bot:  hintPinnedClaudeCodeBot,
			seed: func(t *testing.T, p *Publisher) {
				seedTeamAnthropic(t, p)
				seedOAuthKind(t, p.oauthForfait, p.sealer, secrets.OrgOwnerKey("team1"), secrets.OAuthKindClaudeCode)
			},
		},
		{
			name: "a ChatGPT forfait beside the team's OpenAI key",
			bot:  openAIClawBot,
			seed: func(t *testing.T, p *Publisher) {
				seedTeamOpenAI(t, p)
				seedOAuthKind(t, p.oauthForfait, p.sealer, secrets.OrgOwnerKey("team1"), secrets.OAuthKindCodex)
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			p := forkFundingPublisher(t)
			row.seed(t, p)
			wf := compileTestSource(t, row.bot)
			ctx := store.WithTenant(context.Background(), "team1")
			for _, trust := range []store.RunTrust{store.RunTrustFork, store.RunTrustDefault} {
				spec := forkFundingSpec(trust)
				spec.LLMRoutePolicy = resolvedDefaultPolicy()
				if err := p.FundingRefusal(ctx, spec, wf); err != nil {
					t.Fatalf("trust %q: FundingRefusal = %v, want nil — the team's own key funds the route, and no forfait rung lands on a fork run", trust, err)
				}
			}
			res, err := p.resolveAndSealCredentials(ctx, "run-fork-ladder", "org1", "team1", "owner1", "review-pr", "forks", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustFork, nil, resolvedDefaultPolicy(), nil)
			if err != nil || res.secretsRef == "" {
				t.Fatalf("resolveAndSealCredentials = (ref %q, %v), want a sealed bundle — the team's own key funds the route", res.secretsRef, err)
			}
			for _, st := range res.Ladder {
				if st.Provider == llmroute.CredClaudeForfait || st.Provider == llmroute.CredChatGPTForfait {
					t.Fatalf("the fork run's ladder carries the forfait stage %+v — a forfait funds no fork run", st)
				}
			}
			ctrl, cerr := p.resolveAndSealCredentials(ctx, "run-trusted-ladder", "org1", "team1", "owner1", "review-pr", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, resolvedDefaultPolicy(), nil)
			if cerr != nil || ctrl.secretsRef == "" {
				t.Fatalf("trusted control: resolveAndSealCredentials = (ref %q, %v), want a sealed bundle", ctrl.secretsRef, cerr)
			}
			forfaitStage := false
			for _, st := range ctrl.Ladder {
				forfaitStage = forfaitStage || st.Provider == llmroute.CredClaudeForfait || st.Provider == llmroute.CredChatGPTForfait
			}
			if !forfaitStage {
				t.Fatalf("trusted control: ladder %+v carries no forfait stage — the fixture must hold a forfait pair the trusted run lands", ctrl.Ladder)
			}
		})
	}
}

// A claude_code route hinted off the wire, under a providers allowlist that
// admits no anthropic-wire provider: the route still walks the wire's default
// on the runner and finds no slot it may spend (none is ever sealed), so it is
// refused before metering and at publish, named with its allowlist reason.
// The trusted control on the same stores and policy starts.
//
// Mutations, each seen red and restored byte-identical:
//   - endRoute no longer committing the empty site → the judge admits and
//     the publish seals;
//   - unfundedWireDefault skipping an empty slot list → the same.
func TestFundingRefusal_AnAllowlistLeavingAWireRouteNoSlotRefusesTheForkRun(t *testing.T) {
	p := forkFundingPublisher(t)
	seedTeamOpenAI(t, p)
	wf := compileTestSource(t, offWireClaudeCodeBot)
	ctx := store.WithTenant(context.Background(), "team1")
	policy := &store.RunLLMRoutePolicy{Providers: []string{"openai"}, PairOrder: []string{llmroute.Pair(llmroute.HarnessClaw, "openai_key")}}

	spec := forkFundingSpec(store.RunTrustFork)
	spec.LLMRoutePolicy = policy
	err := p.FundingRefusal(ctx, spec, wf)
	var refusal *UnfundedProvidersError
	if !errors.As(err, &refusal) || !refusal.Untrusted || len(refusal.WireDefault) != 1 || refusal.WireDefault[0] != "draft (no slot the providers allowlist admits)" {
		t.Fatalf("FundingRefusal = %v, want the untrusted refusal naming draft's wire-default read the allowlist leaves empty", err)
	}
	res, err := p.resolveAndSealCredentials(ctx, "run-fork-allowlist", "org1", "team1", "owner1", "review-pr", "forks", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustFork, nil, policy, nil)
	if !errors.As(err, &refusal) || len(refusal.WireDefault) != 1 || res.secretsRef != "" {
		t.Fatalf("resolveAndSealCredentials = (ref %q, %v), want the wire-default refusal and nothing sealed", res.secretsRef, err)
	}
	ctrlSpec := forkFundingSpec(store.RunTrustDefault)
	ctrlSpec.LLMRoutePolicy = policy
	if cerr := p.FundingRefusal(ctx, ctrlSpec, wf); cerr != nil {
		t.Fatalf("trusted control: FundingRefusal = %v, want nil", cerr)
	}
}
