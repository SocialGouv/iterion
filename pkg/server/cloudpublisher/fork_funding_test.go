package cloudpublisher

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// The fork run's funding (the operator's decision on #3125): a pool-stamped
// fork run is sealed no shared tier (ADR-154 D13) and spends only its team's
// own API keys; the runner refuses its pod's env and a forfait cannot cross
// its sandbox (ADR-162 decision 7). The publisher's unfunded decision arms
// the untrusted posture, and the funding judges the launch gate asks before
// metering ARE the credential preview reading that decision.

// gatewayOnlyBot rides the runner's openai_compatible gateway alone: the
// env-funded program (#2038) the walk acquires nothing for.
const gatewayOnlyBot = "agent draft:\n  model: \"openai_compatible/glm-5.3\"\n  backend: \"claw\"\n\nworkflow main:\n  draft -> done\n"

// gatewayBesideVendorBot names a vendor route the team's key funds beside a
// gateway route: not env-funded, so only the gateway arm can see it.
const gatewayBesideVendorBot = "agent draft:\n  model: \"anthropic/claude-opus-5\"\n  backend: \"claw\"\n\nagent second:\n  model: \"openai_compatible/glm-5.3\"\n  backend: \"claw\"\n\nworkflow main:\n  draft -> second\n  second -> done\n"

// envDialBot reads its model from a routing dial, review-pr's shape: this
// process expands it to a vendor route the team's key funds, while a fork
// pool's runner expands it with the pool's own env block.
const envDialBot = "agent draft:\n  model: \"${ITERION_FORK_FUNDING_PROBE_MODEL:-anthropic/claude-opus-5}\"\n  backend: \"claw\"\n\nworkflow main:\n  draft -> done\n"

// envGatewayDialBot reads a dial this process expands to the gateway (the
// test sets it), beside a funded vendor route.
const envGatewayDialBot = "agent draft:\n  model: \"anthropic/claude-opus-5\"\n  backend: \"claw\"\n\nagent second:\n  model: \"${ITERION_FORK_FUNDING_PROBE_GATEWAY:-anthropic/claude-opus-5}\"\n  backend: \"claw\"\n\nworkflow main:\n  draft -> second\n  second -> done\n"

// unattributedBesideVendorBot names a vendor route the team's OpenAI key
// funds beside a claude_code route with no model: the walk attributes the
// second to no provider, and on the runner it reads the anthropic wire's
// default slots.
const unattributedBesideVendorBot = "agent draft:\n  model: \"openai/gpt-5.4-mini\"\n  backend: \"claw\"\n\nagent second:\n  backend: \"claude_code\"\n\nworkflow main:\n  draft -> second\n  second -> done\n"

// bareClaudeCodeBot is a claude_code route on a prefix-less model: the walk
// attributes it to no provider.
const bareClaudeCodeBot = "agent draft:\n  model: \"claude-opus-5\"\n  backend: \"claude_code\"\n\nworkflow main:\n  draft -> done\n"

// offWireClaudeCodeBot pins claude_code to a provider off the anthropic wire:
// the walk names openai, while the delegate reads the wire's default slots.
const offWireClaudeCodeBot = "agent draft:\n  model: \"gpt-5.4-mini\"\n  provider: \"openai\"\n  backend: \"claude_code\"\n\nworkflow main:\n  draft -> done\n"

// backendlessOpenAIBot names no backend: the runner picks one (its
// ITERION_DEFAULT_BACKEND, its credential detection), so the route may read
// the anthropic wire's default slots.
const backendlessOpenAIBot = "agent draft:\n  model: \"openai/gpt-5.4-mini\"\n\nworkflow main:\n  draft -> done\n"

// prefixedClaudeCodeBot is a claude_code route on a provider prefix: it names
// anthropic and reads the wire's default slots.
const prefixedClaudeCodeBot = "agent draft:\n  model: \"anthropic/claude-opus-5\"\n  backend: \"claude_code\"\n\nworkflow main:\n  draft -> done\n"

// glmClaudeCodeBot is a hint-less GLM id on claude_code: it names zai and
// reads every wire slot but zai's.
const glmClaudeCodeBot = "agent draft:\n  model: \"glm-5.3\"\n  backend: \"claude_code\"\n\nworkflow main:\n  draft -> done\n"

// seedTeamAnthropic gives team1 its own anthropic key.
func seedTeamAnthropic(t *testing.T, p *Publisher) {
	t.Helper()
	seedKeyFP(t, p.apiKeys, p.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team", "fp-ant-team")
}

// seedTeamOpenAI gives team1 its own openai key.
func seedTeamOpenAI(t *testing.T, p *Publisher) {
	t.Helper()
	seedKeyFP(t, p.apiKeys, p.sealer, "team1", secrets.ProviderOpenAI, "sk-oai-team", "fp-oai-team")
}

// seedTeamZAI gives team1 its own z.ai key.
func seedTeamZAI(t *testing.T, p *Publisher) {
	t.Helper()
	seedKeyFP(t, p.apiKeys, p.sealer, "team1", secrets.ProviderZAI, "sk-zai-team", "fp-zai-team")
}

// forkFundingPublisher is the real publisher over in-memory stores: team1
// belongs to org1, whose shared credentials every team of the org may spend.
func forkFundingPublisher(t *testing.T) *Publisher {
	t.Helper()
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	var buf bytes.Buffer
	return &Publisher{apiKeys: secrets.NewMemoryApiKeyStore(), oauthForfait: secrets.NewMemoryOAuthStore(),
		usageCaps: usagecap.NewMemStore(), runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer,
		logger: iterlog.New(iterlog.LevelInfo, &buf), pools: activeRegistry("forks"),
		identity: &fakeTeamResolver{orgs: map[string]string{"team1": "org1"}, runnerPool: map[string]string{},
			orgDocs: map[string]identity.Org{"org1": {ID: "org1", CredentialAudience: identity.CredentialAudience{AllTeams: true}}}}}
}

// seedSharedTiers gives org1 and the platform an anthropic key each: the
// shared funding a trusted run of team1 spends and a fork run never may.
func seedSharedTiers(t *testing.T, p *Publisher) {
	t.Helper()
	seedKeyFP(t, p.apiKeys, p.sealer, secrets.OrgTierTenantID("org1"), secrets.ProviderAnthropic, "sk-ant-org", "fp-ant-org")
	seedKeyFP(t, p.apiKeys, p.sealer, secrets.PlatformTenantID, secrets.ProviderAnthropic, "sk-ant-platform", "fp-ant-platform")
}

// forkFundingSpec is the launch a fork-lane gesture hands the judge, or its
// trusted twin (same team, bot and program, no lane pool).
func forkFundingSpec(trust store.RunTrust) runview.CredentialPreviewSpec {
	spec := previewSpec("team1", "owner1")
	spec.Launch.Trust = trust
	if !trust.Trusted() {
		spec.Launch.ForkLanePool = "forks"
	}
	return spec
}

// One row per guard, each isolated by its fixture, each judged on the REAL
// producer (FundingRefusal = the credential preview + unfundedRefusalFor),
// and each with its trusted control on the SAME stores — so a refusal is the
// fork posture, never a credential the fixture forgot.
//
// Mutations, each seen red and restored byte-identical:
//   - unfundedPostureFor no longer arming Untrusted → the three refused rows
//     admit (the posture is what refuses them all);
//   - the preview resolving an untrusted run's pool from its team's mapping
//     (the trusted arm) → "funded only by the org and the platform" admits,
//     the shared tiers funding it; the forfait and gateway rows, still
//     refused, lose the lane's pool from their sentence;
//   - fundedProviderSet counting a forfait under the untrusted posture →
//     "a forfait" admits, alone;
//   - the untrusted arm exempting an env-funded program (`&& !envFunded`) →
//     "an env-funded program" admits, alone;
//   - the untrusted arm ignoring routes.gateway → the two gateway rows admit
//     ("an env-funded program" falls to the unattributable sentence, red on
//     its wording);
//   - the untrusted arm ignoring routes.deferred → "a routing dial" admits;
//   - the untrusted arm reading every attributed provider instead of the
//     decided ones → "a routing dial on a team holding nothing" names a key
//     to provision;
//   - the untrusted arm ignoring the unattributed sites → the three
//     unattributed rows admit (the program holding nothing falls to the
//     Unattributable arm, red on its wording);
//   - unfundedWireDefault answering nothing → the two unfunded wire-default
//     rows admit;
//   - unfundedWireDefault's slot leg dropped → "a slot it reads" is refused;
//   - unfundedWireDefault's named-provider leg dropped → "a GLM id its
//     team's z.ai key serves" is refused.
func TestFundingRefusal_AForkRunSpendsItsTeamsOwnKeysAlone(t *testing.T) {
	t.Setenv("ITERION_FORK_FUNDING_PROBE_GATEWAY", "openai_compatible/glm-5.3")
	for _, row := range []struct {
		name          string
		bot           string
		seed          func(t *testing.T, p *Publisher)
		refused       bool
		wantInRefusal string
		notInRefusal  string
	}{
		{
			name:    "a team funded only by the org and the platform is refused",
			bot:     anthropicPinnedBot,
			seed:    seedSharedTiers,
			refused: true, wantInRefusal: "provision a team API key for: anthropic",
		},
		{
			name: "the team's own key funds it",
			bot:  anthropicPinnedBot,
			seed: func(t *testing.T, p *Publisher) {
				seedSharedTiers(t, p)
				seedKeyFP(t, p.apiKeys, p.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team", "fp-ant-team")
			},
		},
		{
			name: "a forfait funds no fork run",
			bot:  anthropicPinnedBot,
			seed: func(t *testing.T, p *Publisher) {
				seedOAuthKind(t, p.oauthForfait, p.sealer, secrets.OrgOwnerKey("team1"), secrets.OAuthKindClaudeCode)
			},
			refused: true, wantInRefusal: "a forfait cannot cross the run's sandbox",
		},
		{
			name:    "an env-funded program is refused: its gateway is the refused environment",
			bot:     gatewayOnlyBot,
			seed:    seedTeamAnthropic,
			refused: true, wantInRefusal: "the routes of draft ride the openai_compatible gateway",
		},
		{
			name:    "a gateway route beside a funded vendor route is refused",
			bot:     gatewayBesideVendorBot,
			seed:    seedTeamAnthropic,
			refused: true, wantInRefusal: "the routes of second ride the openai_compatible gateway",
		},
		{
			name:    "a routing dial this process expands to the gateway is refused as both",
			bot:     envGatewayDialBot,
			seed:    seedTeamAnthropic,
			refused: true, wantInRefusal: "the routes of second ride the openai_compatible gateway",
		},
		{
			name:    "a routing dial the pool's runner expands is refused, though this process reads a funded vendor",
			bot:     envDialBot,
			seed:    seedTeamAnthropic,
			refused: true, wantInRefusal: "the routes of draft are decided on the runner",
		},
		{
			name:    "a routing dial on a team holding nothing names the dial, not a key to provision",
			bot:     envDialBot,
			seed:    func(*testing.T, *Publisher) {},
			refused: true, wantInRefusal: "the routes of draft are decided on the runner",
			notInRefusal: "provision a team API key for",
		},
		{
			name:    "an unattributable program on a team holding nothing",
			bot:     unattributableBot,
			seed:    func(*testing.T, *Publisher) {},
			refused: true, wantInRefusal: "the routes of draft name no provider this launch can read",
		},
		{
			name:    "an unattributed route beside a vendor route the team's key funds is refused",
			bot:     unattributedBesideVendorBot,
			seed:    seedTeamOpenAI,
			refused: true, wantInRefusal: "the routes of second name no provider this launch can read",
			notInRefusal: "provision a team API key for",
		},
		{
			name:    "a prefix-less claude_code model is refused, though the team's Anthropic key would serve it",
			bot:     bareClaudeCodeBot,
			seed:    seedTeamAnthropic,
			refused: true, wantInRefusal: "the routes of draft name no provider this launch can read",
		},
		{
			name:    "a claude_code route pinned off the wire reads a wire default the team holds no key for",
			bot:     offWireClaudeCodeBot,
			seed:    seedTeamOpenAI,
			refused: true, wantInRefusal: "the routes of draft (anthropic, moonshot, zai) may spend the anthropic wire's default credential",
		},
		{
			name:    "a route whose backend the runner picks reads a wire default the team holds no key for",
			bot:     backendlessOpenAIBot,
			seed:    seedTeamOpenAI,
			refused: true, wantInRefusal: "the routes of draft (anthropic, moonshot, zai) may spend the anthropic wire's default credential",
		},
		{
			name: "a wire-default route is funded by a slot it reads",
			bot:  offWireClaudeCodeBot,
			seed: func(t *testing.T, p *Publisher) {
				seedTeamOpenAI(t, p)
				seedTeamAnthropic(t, p)
			},
		},
		{
			name: "a claude_code route on a provider prefix the team's key funds",
			bot:  prefixedClaudeCodeBot,
			seed: seedTeamAnthropic,
		},
		{
			name: "a GLM id its team's z.ai key serves",
			bot:  glmClaudeCodeBot,
			seed: seedTeamZAI,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			p := forkFundingPublisher(t)
			row.seed(t, p)
			wf := compileTestSource(t, row.bot)
			ctx := store.WithTenant(context.Background(), "team1")

			err := p.FundingRefusal(ctx, forkFundingSpec(store.RunTrustFork), wf)
			if !row.refused {
				if err != nil {
					t.Fatalf("FundingRefusal = %v, want the fork run admitted on its team's own key", err)
				}
			} else {
				var refusal *UnfundedProvidersError
				if !errors.As(err, &refusal) || !refusal.Untrusted || !errors.Is(err, runview.ErrNoLLMCredential) {
					t.Fatalf("FundingRefusal = %v, want the typed untrusted refusal (*UnfundedProvidersError, Untrusted, ErrNoLLMCredential)", err)
				}
				if !strings.Contains(err.Error(), row.wantInRefusal) || !strings.Contains(err.Error(), `runner pool "forks"`) {
					t.Fatalf("refusal %q, want it naming %q and the lane's pool", err, row.wantInRefusal)
				}
				if row.notInRefusal != "" && strings.Contains(err.Error(), row.notInRefusal) {
					t.Fatalf("refusal %q names %q — the refusal names only what the run cannot fund", err, row.notInRefusal)
				}
			}
			// The trusted control: the same team, stores and program fund a
			// trusted launch — the refusal above is the fork posture alone.
			if cerr := p.FundingRefusal(ctx, forkFundingSpec(store.RunTrustDefault), wf); cerr != nil {
				t.Fatalf("trusted control: FundingRefusal = %v, want nil — the fixture must fund a trusted run", cerr)
			}
		})
	}
}

// The live launch refuses the same shape at publish, through the same
// producer (the walk's unfundedRefusalFor on the sealed bundle): an untrusted
// run no key of its team's funds is refused typed, and nothing is sealed.
// The gate's pre-metering refusal is the first floor; this one stands for a
// carrier that reaches the publisher unjudged (a launch built from a fork
// run), whose handler then refunds the gate.
//
// Mutation: the walk's posture read with the trusted default instead of the
// run's trust → D13 still keeps the shared tiers out, nothing refuses, and
// the run walks on with no bundle (red on the refusal assertion, both rows).
func TestResolveAndSeal_AnUnfundedUntrustedRunIsRefusedAtPublish(t *testing.T) {
	for _, row := range []struct {
		name    string
		require bool
	}{
		{"a default deployment", false},
		{"under the RequireLLMCredential knob: the untrusted remedy still answers", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			p := forkFundingPublisher(t)
			p.requireLLMCredential = row.require
			seedSharedTiers(t, p)
			wf := compileTestSource(t, anthropicPinnedBot)
			ctx := store.WithTenant(context.Background(), "team1")
			res, err := p.resolveAndSealCredentials(ctx, "run-unfunded-fork", "org1", "team1", "owner1", "review-pr", "forks", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustFork, nil, nil, nil, boundaryDerived)
			var refusal *UnfundedProvidersError
			if !errors.As(err, &refusal) || !refusal.Untrusted || !errors.Is(err, runview.ErrNoLLMCredential) {
				t.Fatalf("resolveAndSealCredentials = %v, want the typed untrusted refusal", err)
			}
			if strings.Contains(err.Error(), "ITERION_CLOUD_REQUIRE_LLM_CREDENTIAL") {
				t.Fatalf("refusal %q offers the runner's env as a remedy — an untrusted run is refused that env", err)
			}
			if res.secretsRef != "" {
				t.Fatalf("a refused fork run sealed a bundle (ref=%q)", res.secretsRef)
			}
		})
	}
}

// The publish-time floor of the same route classes: a fork run whose team's
// own key funds its vendor route but which ALSO rides the gateway, reads a
// routing dial the pool's runner expands, takes a route the walk cannot
// attribute, or reads a wire default its team holds no slot of, is refused at
// publish and seals nothing — the bundle a runner would then refuse ambient
// after the gate metered it. The trusted control on the same stores seals.
//
// Mutations, each red on its own rows (the refusal assertion) and restored
// byte-identical:
//   - the untrusted arm ignoring routes.gateway and routes.deferred → the
//     gateway and dial rows seal a bundle;
//   - the untrusted arm ignoring the unattributed sites → the two
//     unattributed rows seal;
//   - unfundedWireDefault answering nothing → the two wire-default rows seal.
func TestResolveAndSeal_AForkRunsGatewayOrDeferredRouteIsRefusedAtPublish(t *testing.T) {
	gatewayOrDeferred := func(r *UnfundedProvidersError) bool { return len(r.Gateway)+len(r.Deferred) > 0 }
	unattributed := func(r *UnfundedProvidersError) bool { return len(r.Unattributed) > 0 }
	wireDefault := func(r *UnfundedProvidersError) bool { return len(r.WireDefault) > 0 }
	for _, row := range []struct {
		name, bot string
		seed      func(t *testing.T, p *Publisher)
		class     func(*UnfundedProvidersError) bool
	}{
		{"a gateway route beside a funded vendor route", gatewayBesideVendorBot, seedTeamAnthropic, gatewayOrDeferred},
		{"a routing dial the pool's runner expands", envDialBot, seedTeamAnthropic, gatewayOrDeferred},
		{"an unattributed route beside a funded vendor route", unattributedBesideVendorBot, seedTeamOpenAI, unattributed},
		{"a prefix-less claude_code model", bareClaudeCodeBot, seedTeamAnthropic, unattributed},
		{"a claude_code route pinned off the wire", offWireClaudeCodeBot, seedTeamOpenAI, wireDefault},
		{"a route whose backend the runner picks", backendlessOpenAIBot, seedTeamOpenAI, wireDefault},
	} {
		t.Run(row.name, func(t *testing.T) {
			p := forkFundingPublisher(t)
			row.seed(t, p)
			wf := compileTestSource(t, row.bot)
			ctx := store.WithTenant(context.Background(), "team1")
			res, err := p.resolveAndSealCredentials(ctx, "run-fork-routes", "org1", "team1", "owner1", "review-pr", "forks", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustFork, nil, nil, nil, boundaryDerived)
			var refusal *UnfundedProvidersError
			if !errors.As(err, &refusal) || !refusal.Untrusted || !row.class(refusal) {
				t.Fatalf("resolveAndSealCredentials = %v, want the typed untrusted refusal naming the row's route class", err)
			}
			if res.secretsRef != "" {
				t.Fatalf("a refused fork run sealed a bundle (ref=%q)", res.secretsRef)
			}
			ctrl, cerr := p.resolveAndSealCredentials(ctx, "run-trusted-routes", "org1", "team1", "owner1", "review-pr", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, nil, nil, boundaryDerived)
			if cerr != nil || ctrl.secretsRef == "" {
				t.Fatalf("trusted control: resolveAndSealCredentials = (ref %q, %v), want a sealed bundle — the fixture must fund a trusted run", ctrl.secretsRef, cerr)
			}
		})
	}
}

// A fork run's resume is judged on its FROZEN program, trust and pool stamp:
// a team that lost its own key since the launch is refused before the resume
// is metered, and the same run resumes once the key is back.
//
// Mutation: ResumeFundingRefusal judging the run as trusted (Launch.Trust
// left unset) → the shared tiers fund the run and "the team lost its key"
// admits.
func TestResumeFundingRefusal_AForkRunWhoseTeamLostItsKeyIsRefused(t *testing.T) {
	prior := &store.Run{
		ID: "run-fork", TenantID: "team1", OwnerID: "owner1", BotID: "review-pr",
		Trust: store.RunTrustFork, RunnerPool: "forks", PinnedProviders: []string{"anthropic"},
		ProjectPath: "acme/widgets",
	}
	wf := compileTestSource(t, anthropicPinnedBot)

	t.Run("the team lost its key: refused, typed", func(t *testing.T) {
		p := forkFundingPublisher(t)
		seedSharedTiers(t, p)
		err := p.ResumeFundingRefusal(context.Background(), prior, wf)
		var refusal *UnfundedProvidersError
		if !errors.As(err, &refusal) || !refusal.Untrusted || refusal.Pool != "forks" {
			t.Fatalf("ResumeFundingRefusal = %v, want the typed untrusted refusal on the frozen pool", err)
		}
	})
	t.Run("the key is back: the resume is funded", func(t *testing.T) {
		p := forkFundingPublisher(t)
		seedKeyFP(t, p.apiKeys, p.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team", "fp-ant-team")
		if err := p.ResumeFundingRefusal(context.Background(), prior, wf); err != nil {
			t.Fatalf("ResumeFundingRefusal = %v, want nil", err)
		}
	})
}

// The fork lane's POOL rule answers in the funding judges too, before the
// funding walk: the gate asks them before metering, so a pool the launch or
// the resume would refuse at publish — after the gate charged — is refused
// there instead. Each row's team holds its own key (the run is funded), so
// the refusal is the pool's alone; each control is the same fixture on the
// lane's own pool.
//
// Mutations, each seen red and restored byte-identical:
//   - FundingRefusal no longer reading launchRunnerPool → "a team mapped to
//     another pool" and "a pool another team holds" admit;
//   - FundingRefusal no longer asking the registry's routing gate → every
//     registry row admits;
//   - routesForTrust no longer typing the registry's decided refusal → "a
//     pool the registry does not know" and "a disabled pool" answer the
//     untyped (transient) refusal;
//   - routesForTrust typing every registry error → "a registry that cannot
//     be read" answers ErrForkLanePool (a blip read as permanent);
//   - routesForTrust typing a pool in transition (the errPoolInTransition
//     carve-out dropped) → "a draining pool" and "a provisioning pool"
//     answer ErrForkLanePool, their parked runs abandoned;
//   - poolRoutes marking every non-active state in transition → "a disabled
//     pool" answers the transient refusal;
//   - ResumeFundingRefusal no longer asking resumePoolRefusal → "the team was
//     mapped since the launch" admits.
func TestFundingJudges_TheForkLanePoolRuleAnswersBeforeTheFundingWalk(t *testing.T) {
	ctx := store.WithTenant(context.Background(), "team1")
	wf := compileTestSource(t, anthropicPinnedBot)
	funded := func(t *testing.T, mapped map[string]string) *Publisher {
		p := forkFundingPublisher(t)
		seedTeamAnthropic(t, p)
		for team, pool := range mapped {
			p.identity.(*fakeTeamResolver).runnerPool[team] = pool
		}
		return p
	}
	for _, row := range []struct {
		name     string
		mapped   map[string]string
		lanePool string
		registry RunnerPoolResolver
		typed    bool
		naming   string
		// notRouted: the registry's transient decided refusal
		// (ErrPoolNotRouted, untyped for the fork lane).
		notRouted bool
	}{
		{name: "a team mapped to another pool", mapped: map[string]string{"team1": "t-sov"}, lanePool: "forks", typed: true},
		{name: "a pool another team holds", mapped: map[string]string{"team2": "forks"}, lanePool: "forks", typed: true},
		// The config names the pool and is immutable: a definitive refusal,
		// never the retryable blip.
		{name: "a pool the registry does not know", lanePool: "unregistered", typed: true, naming: `"unregistered"`},
		{name: "a disabled pool", lanePool: "forks", typed: true, naming: `"disabled"`,
			registry: fakePoolRegistry{rec: &platformcfg.RunnerPools{Pools: []platformcfg.RunnerPool{{Name: "forks", State: platformcfg.RunnerPoolDisabled}}}}},
		// A lifecycle step the operator moves through: refused, but as the
		// transient a trusted run meets — the next state change cures it.
		{name: "a draining pool", lanePool: "forks", naming: `"draining"`, notRouted: true,
			registry: fakePoolRegistry{rec: &platformcfg.RunnerPools{Pools: []platformcfg.RunnerPool{{Name: "forks", State: platformcfg.RunnerPoolDraining}}}}},
		{name: "a provisioning pool", lanePool: "forks", naming: `"provisioning"`, notRouted: true,
			registry: fakePoolRegistry{rec: &platformcfg.RunnerPools{Pools: []platformcfg.RunnerPool{{Name: "forks", State: platformcfg.RunnerPoolProvisioning}}}}},
		{name: "a registry that cannot be read", lanePool: "forks", naming: "registry unreachable",
			registry: fakePoolRegistry{err: errors.New("registry unreachable")}},
	} {
		t.Run(row.name, func(t *testing.T) {
			p := funded(t, row.mapped)
			if row.registry != nil {
				p.pools = row.registry
			}
			spec := forkFundingSpec(store.RunTrustFork)
			spec.Launch.ForkLanePool = row.lanePool
			err := p.FundingRefusal(ctx, spec, wf)
			if err == nil || errors.Is(err, runview.ErrNoLLMCredential) {
				t.Fatalf("FundingRefusal = %v, want the pool's refusal", err)
			}
			if errors.Is(err, ErrForkLanePool) != row.typed {
				t.Fatalf("FundingRefusal = %v, ErrForkLanePool=%v, want %v", err, errors.Is(err, ErrForkLanePool), row.typed)
			}
			if row.naming != "" && !strings.Contains(err.Error(), row.naming) {
				t.Fatalf("FundingRefusal = %v, want the registry's refusal naming %s", err, row.naming)
			}
			if row.notRouted && !errors.Is(err, ErrPoolNotRouted) {
				t.Fatalf("FundingRefusal = %v, want the transient ErrPoolNotRouted", err)
			}
		})
	}
	t.Run("control: the lane's own pool, unheld and active", func(t *testing.T) {
		if err := funded(t, nil).FundingRefusal(ctx, forkFundingSpec(store.RunTrustFork), wf); err != nil {
			t.Fatalf("FundingRefusal = %v, want nil", err)
		}
	})
	prior := &store.Run{
		ID: "run-fork", TenantID: "team1", OwnerID: "owner1", BotID: "review-pr",
		Trust: store.RunTrustFork, RunnerPool: "forks", PinnedProviders: []string{"anthropic"},
	}
	t.Run("the team was mapped since the launch: the resume is refused", func(t *testing.T) {
		err := funded(t, map[string]string{"team1": "t-sov"}).ResumeFundingRefusal(context.Background(), prior, wf)
		if !errors.Is(err, ErrForkLanePool) {
			t.Fatalf("ResumeFundingRefusal = %v, want ErrForkLanePool", err)
		}
	})
	t.Run("control: the resume on its frozen, unheld pool", func(t *testing.T) {
		if err := funded(t, nil).ResumeFundingRefusal(context.Background(), prior, wf); err != nil {
			t.Fatalf("ResumeFundingRefusal = %v, want nil", err)
		}
	})
}

// The funding readers drop a forfait under the untrusted posture, on BOTH
// sources (the sealed bundle and the preview's predicted fills), and keep it
// for every other posture.
//
// Mutation: fundedProviderSet's untrusted return removed → the untrusted
// reading names anthropic (red), on both sides at once.
func TestFundingReaders_AForfaitFundsNoUntrustedRun(t *testing.T) {
	x := &credentialPreview{
		api:   map[secrets.Provider]int{secrets.ProviderZAI: 0},
		oauth: map[string]int{string(secrets.OAuthKindClaudeCode): 1},
	}
	bundle := secrets.RunBundle{
		APIKeys:          map[secrets.Provider]string{secrets.ProviderZAI: "sk-zai"},
		OAuthCredentials: map[string][]byte{string(secrets.OAuthKindClaudeCode): []byte("blob")},
	}
	untrusted := unfundedPosture{Untrusted: true}
	for name, got := range map[string]map[string]bool{
		"bundle":   bundleFunding(bundle, untrusted),
		"metadata": metadataFunding(x, untrusted),
	} {
		if got["anthropic"] || !got["zai"] || len(got) != 1 {
			t.Errorf("%s funding under the untrusted posture = %v, want the key alone (zai) — a forfait cannot cross the sandbox", name, got)
		}
	}
	if got := bundleFunding(bundle, unfundedPosture{}); !got["anthropic"] {
		t.Errorf("trusted control: bundle funding = %v, want the forfait's anthropic", got)
	}
}

// The deferred and unattributed remedies say how far an integration's routing
// override reaches — a node's own route, nothing else — so an operator facing
// a supervisor, a sub-bot or a fallback route is pointed at the bot's text or
// the launch's fallback chain rather than at an override that cannot lift the
// refusal. Judged on the gate's judge, one row per remedy.
//
// Mutation: either remedy's sentence back to "the integration's routing
// overrides pin a node's provider, model and backend" (overrideReach dropped)
// → its row is red on the reach assertion.
func TestFundingRefusal_TheRemedyNamesWhatARoutingOverrideReaches(t *testing.T) {
	for _, row := range []struct {
		name, bot, remedy string
		seed              func(t *testing.T, p *Publisher)
	}{
		{"an unattributed route", unattributedBesideVendorBot, "the routes of second name no provider this launch can read", seedTeamOpenAI},
		{"a deferred route", envDialBot, "the routes of draft are decided on the runner", seedTeamAnthropic},
	} {
		t.Run(row.name, func(t *testing.T) {
			p := forkFundingPublisher(t)
			row.seed(t, p)
			wf := compileTestSource(t, row.bot)
			err := p.FundingRefusal(store.WithTenant(context.Background(), "team1"), forkFundingSpec(store.RunTrustFork), wf)
			if err == nil {
				t.Fatal("FundingRefusal = nil, want the untrusted refusal")
			}
			msg := err.Error()
			start := strings.Index(msg, row.remedy)
			if start < 0 {
				t.Fatalf("refusal %q, want the remedy %q", msg, row.remedy)
			}
			remedy := msg[start:]
			if end := strings.Index(remedy, "; and "); end >= 0 {
				remedy = remedy[:end]
			}
			if !strings.Contains(remedy, "pin a node's own route (provider, model and backend) and nothing else") || !strings.Contains(remedy, "a supervisor") {
				t.Fatalf("remedy %q does not say an override reaches a node's own route alone", remedy)
			}
		})
	}
}
