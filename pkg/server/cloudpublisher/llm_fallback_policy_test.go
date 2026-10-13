package cloudpublisher

// Team.LLMFallback is the team's credential-fallback policy. The empty value
// (and "platform") keeps today's walk — the shared tiers stay the fallback
// chain. "none" is the sovereign posture: the launch consults ONLY the
// team's own credentials, an LLM route nothing of the team's funds refuses
// the launch by name, and no shared tier (org, pool, platform) ever seals a
// credential into the team's bundle.

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

// llmFallbackPublisher builds the minimal publisher for a policy test: an
// empty BYOK store, the identity resolver answering the policy, nothing else.
func llmFallbackPublisher(t *testing.T, llmFallback map[string]string) (*Publisher, *bytes.Buffer) {
	t.Helper()
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	var buf bytes.Buffer
	p := &Publisher{apiKeys: secrets.NewMemoryApiKeyStore(), oauthForfait: secrets.NewMemoryOAuthStore(),
		usageCaps: usagecap.NewMemStore(), runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer,
		logger:   iterlog.New(iterlog.LevelInfo, &buf),
		identity: &fakeTeamResolver{orgs: map[string]string{"team1": ""}, llmFallback: llmFallback}}
	return p, &buf
}

func resolvePolicy(t *testing.T, p *Publisher, src string) error {
	t.Helper()
	wf := compileTestSource(t, src)
	ctx := store.WithTenant(context.Background(), "team1")
	_, err := p.resolveAndSealCredentials(ctx, "run-pol", "", "team1", "owner1", "", "",
		wf, nil, nil, model.ModelOverrides{}, nil, store.RunTrustDefault, nil, nil)
	return err
}

// A "none" team with NO credential of its own is refused even though the
// platform tier holds the key — that is the leak this policy exists to
// close: the shared tier never seals into the team's runs.
func TestResolve_LLMFallbackNone_SkipsSharedTiers(t *testing.T) {
	p, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})
	seedKeyFP(t, p.apiKeys, p.sealer, secrets.PlatformTenantID, secrets.ProviderAnthropic, "sk-ant-platform", "fp-ant-platform")

	err := resolvePolicy(t, p, anthropicPinnedBot)
	if !errors.Is(err, runview.ErrNoLLMCredential) {
		t.Fatalf("resolveAndSealCredentials = %v, want runview.ErrNoLLMCredential — the platform key must not fund a none team", err)
	}
	if !strings.Contains(err.Error(), "none") {
		t.Errorf("refusal does not name the policy: %q", err)
	}
}

// Strict per pinned provider: one funded provider does not excuse another
// pinned route — under "none", every pinned provider must be the team's own.
func TestResolve_LLMFallbackNone_StrictPerPinnedProvider(t *testing.T) {
	p, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})
	seedKeyFP(t, p.apiKeys, p.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team", "fp-ant-team")

	err := resolvePolicy(t, p, twoRoutesBot)
	if !errors.Is(err, runview.ErrNoLLMCredential) {
		t.Fatalf("resolveAndSealCredentials = %v, want runview.ErrNoLLMCredential for the unfunded openai route", err)
	}
	if !strings.Contains(err.Error(), "openai") {
		t.Errorf("refusal does not name the unfunded provider: %q", err)
	}
}

// The team's own forfait funds the run — the policy demands OWN credentials,
// not absence of credentials.
func TestResolve_LLMFallbackNone_TeamForfaitFunds(t *testing.T) {
	p, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})
	seedOAuth(t, p.oauthForfait, p.sealer, "owner1", "sk-forfait-team")

	if err := resolvePolicy(t, p, anthropicPinnedBot); err != nil {
		t.Fatalf("resolveAndSealCredentials = %v, want nil — the team's own forfait funds the run", err)
	}
}

// The default (empty policy) keeps today's walk: the platform tier stays the
// fallback chain for a team without its own credentials.
func TestResolve_LLMFallbackDefault_KeepsSharedTiers(t *testing.T) {
	p, _ := llmFallbackPublisher(t, nil)
	seedKeyFP(t, p.apiKeys, p.sealer, secrets.PlatformTenantID, secrets.ProviderAnthropic, "sk-ant-platform", "fp-ant-platform")

	if err := resolvePolicy(t, p, anthropicPinnedBot); err != nil {
		t.Fatalf("resolveAndSealCredentials = %v, want nil — the default must keep the shared tiers", err)
	}
}

// A run that cannot call a model is never refused by the policy.
func TestResolve_LLMFallbackNone_ToolOnlyRunUnaffected(t *testing.T) {
	p, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})

	if err := resolvePolicy(t, p, toolOnlyBot); err != nil {
		t.Fatalf("resolveAndSealCredentials = %v, want nil — a tool-only run spends nothing", err)
	}
}

// An env-funded run (every route rides the runner's gateway) is untouched.
func TestResolve_LLMFallbackNone_EnvFundedRunUnaffected(t *testing.T) {
	p, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})

	gatewayBot := "agent draft:\n  model: \"openai_compatible/glm-5.2\"\n  backend: \"claw\"\n\nworkflow main:\n  draft -> done\n"
	if err := resolvePolicy(t, p, gatewayBot); err != nil {
		t.Fatalf("resolveAndSealCredentials = %v, want nil — an env-funded run acquires no credential", err)
	}
}

// An unknown policy value is never read as the default: the launch refuses
// naming the value, because a typo'd boundary field must not silently
// re-open the shared tiers.
func TestResolve_LLMFallbackUnknownValue_RefusesClosed(t *testing.T) {
	p, _ := llmFallbackPublisher(t, map[string]string{"team1": "sovereign"})
	seedKeyFP(t, p.apiKeys, p.sealer, secrets.PlatformTenantID, secrets.ProviderAnthropic, "sk-ant-platform", "fp-ant-platform")

	err := resolvePolicy(t, p, anthropicPinnedBot)
	if err == nil {
		t.Fatalf("resolveAndSealCredentials = nil, want a refusal naming the unknown value")
	}
	if !strings.Contains(err.Error(), "sovereign") {
		t.Errorf("refusal does not name the unknown value: %q", err)
	}
}

// A route the walk cannot attribute is refused ONLY when the team holds no
// credential at all — the pod's platform env would be the only funding left.
// A team that holds its own credentials keeps the knob's compromise.
func TestResolve_LLMFallbackNone_UnattributableRouteWithoutCredentialRefuses(t *testing.T) {
	p, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})

	err := resolvePolicy(t, p, unattributableBot)
	if !errors.Is(err, runview.ErrNoLLMCredential) {
		t.Fatalf("resolveAndSealCredentials = %v, want runview.ErrNoLLMCredential — nothing of the team's funds this run and no route is checkable", err)
	}
	if !strings.Contains(err.Error(), "llm_fallback=none") {
		t.Errorf("refusal does not name the policy: %q", err)
	}
}

func TestResolve_LLMFallbackNone_UnattributableRouteWithTeamCredentialStarts(t *testing.T) {
	p, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})
	seedKeyFP(t, p.apiKeys, p.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team", "fp-ant-team")

	if err := resolvePolicy(t, p, unattributableBot); err != nil {
		t.Fatalf("resolveAndSealCredentials = %v, want nil — the team's own credential keeps the knob's compromise", err)
	}
}

// The preview applies the same policy the launch does: a "none" team's
// shared tiers show as NOT consulted, with the warning naming the policy —
// never as candidates the launch would refuse to seal.
func TestCredentialPreview_LLMFallbackNone_ShowsSharedTiersUnconsulted(t *testing.T) {
	p, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})
	seedKeyFP(t, p.apiKeys, p.sealer, secrets.PlatformTenantID, secrets.ProviderAnthropic, "sk-ant-platform", "fp-ant-platform")
	seedKeyFP(t, p.apiKeys, p.sealer, secrets.OrgTierTenantID("org-1"), secrets.ProviderAnthropic, "sk-ant-org", "fp-ant-org")
	p.identity = &fakeTeamResolver{orgs: map[string]string{"team1": "org-1"}, llmFallback: map[string]string{"team1": "none"}}

	v, err := p.PreviewCredentials(context.Background(), previewSpec("team1", "owner1"), compileTestSource(t, anthropicPinnedBot))
	if err != nil {
		t.Fatalf("PreviewCredentials: %v", err)
	}
	for _, c := range v.Candidates {
		switch c.Tier {
		case "org", "pool", "platform":
			t.Errorf("candidate tier %q (%s) shown for a none team — the launch refuses to seal it", c.Tier, c.Provider)
		}
	}
	joined := strings.Join(v.Warnings, "\n")
	if !strings.Contains(joined, "llm_fallback=none") {
		t.Errorf("warnings do not name the policy: %v", v.Warnings)
	}
}

// The fallback door is the pool tier wearing another coat: under
// llm_fallback=none a donor's consent does not open it, or a shared
// credential would seal into the sovereign bundle and fund the very
// providers the strict refusal is there to name.
func TestResolve_LLMFallbackNone_ShutsTheFallbackDoor(t *testing.T) {
	f := newDoorFixture(t, true, nil)
	f.pub.identity = &fakeTeamResolver{orgs: map[string]string{poolTeam: poolOrg}, llmFallback: map[string]string{poolTeam: identity.LLMFallbackNone}}
	wf := doorWf()
	bundle, creds := f.resolve(t, "run-door-none", doorPolicy(), wf)

	if creds.doorGrant != nil {
		t.Fatalf("door grant = %+v — the door must stay shut under llm_fallback=none", creds.doorGrant)
	}
	if got := bundle.APIKeys[secrets.ProviderZAI]; got != "" {
		t.Fatalf("sealed zai key = %q — a donor credential sealed into a sovereign bundle", got)
	}
	if len(bundle.PoolSourced) != 0 {
		t.Fatalf("PoolSourced = %v — nothing shared may seal under the policy", bundle.PoolSourced)
	}
}
