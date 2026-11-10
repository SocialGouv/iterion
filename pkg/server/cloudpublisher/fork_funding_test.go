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
		logger: iterlog.New(iterlog.LevelInfo, &buf),
		identity: &fakeTeamResolver{orgs: map[string]string{"team1": "org1"},
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
//     "an env-funded program" admits, alone.
func TestFundingRefusal_AForkRunSpendsItsTeamsOwnKeysAlone(t *testing.T) {
	for _, row := range []struct {
		name          string
		bot           string
		seed          func(t *testing.T, p *Publisher)
		refused       bool
		wantInRefusal string
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
			name: "an env-funded program is refused: its gateway is the refused environment",
			bot:  gatewayOnlyBot,
			seed: func(t *testing.T, p *Publisher) {
				seedKeyFP(t, p.apiKeys, p.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team", "fp-ant-team")
			},
			refused: true, wantInRefusal: "no model route of this workflow names a provider one could fund",
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
			res, err := p.resolveAndSealCredentials(ctx, "run-unfunded-fork", "org1", "team1", "owner1", "review-pr", "forks", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustFork, nil, nil, nil)
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
