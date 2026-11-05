package model

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// clawLeg is the resolver's question for a claw leg on spec, placed in the
// sandbox or on the host.
func clawLeg(spec string, contained bool) credroute.Call {
	return credroute.Call{
		Backend: delegate.BackendClaw, Spec: spec, Role: credroute.RoleNode,
		Sandbox: credroute.Sandbox{Contained: contained},
	}
}

// The claw arm judges the subscription switch on the posture the attempt
// captured (secrets.GuardSubscriptionOAuthIn over the env seam), not on the
// process at the moment of the read: a carried environment that closes the
// switch refuses a forfait-only node while the process leaves it open, and
// one that leaves it open does not. Mutant — the guard's process form
// (secrets.GuardSubscriptionOAuth): the first arm admits.
func TestTheClawArmJudgesTheSwitchOnTheCapturedPosture(t *testing.T) {
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "")
	const spec = "anthropic/claude-sonnet-4"
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): forfaitDirForTest(t)},
	})
	for _, contained := range []bool{false, true} {
		closed := withCarriedEnv(ctx, carriedEnv{"ITERION_FORBID_SUBSCRIPTION_OAUTH": "1"})
		_, err := credroute.Resolve(closed, clawLeg(spec, contained))
		var r *credroute.Refusal
		if !errors.As(err, &r) || r.Reason != credroute.ReasonSubscriptionSwitch || !errors.Is(err, secrets.ErrSubscriptionOAuthForbidden) {
			t.Errorf("contained=%v: the switch the posture closes = %v, want the arm's subscription-switch refusal unwrapping to the site's sentinel", contained, err)
		}
		open := withCarriedEnv(ctx, carriedEnv{"ITERION_FORBID_SUBSCRIPTION_OAUTH": "0"})
		if _, err := credroute.Resolve(open, clawLeg(spec, contained)); errors.As(err, &r) && r.Reason == credroute.ReasonSubscriptionSwitch {
			t.Errorf("contained=%v: a posture that leaves the switch open was refused by it: %v", contained, err)
		}
	}
}

// Every Spend the claw arm names for a run slot carries the slot's audit
// fingerprint — the value the credential plan predicts — in process and
// across the crossing, the run's own key and a key pinned for the route
// alike.
func TestTheClawArmNamesTheSlotItSpendsWithItsFingerprint(t *testing.T) {
	for _, name := range providerCredentialEnvVars {
		t.Setenv(name, "")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Cleanup(SetCredentialsLookup(RunCredentialsLookup))
	const spec = "anthropic/claude-sonnet-4"
	for _, tc := range []struct {
		name  string
		creds secrets.Credentials
		want  credroute.Spend
	}{
		{"the run's own key", secrets.Credentials{
			APIKeys:      map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-run"},
			Fingerprints: map[string]string{"anthropic": "fp-run"},
		}, credroute.Spend{Channel: credroute.ChannelRunKey, Slot: "anthropic", Fingerprint: "fp-run"}},
		{"a key pinned for the route", secrets.Credentials{
			PinnedAPIKeys: map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-pin"},
			Fingerprints:  map[string]string{"anthropic": "fp-pin"},
		}, credroute.Spend{Channel: credroute.ChannelPinnedKey, Slot: "anthropic", Fingerprint: "fp-pin"}},
	} {
		for _, contained := range []bool{false, true} {
			got, err := credroute.Resolve(secrets.WithCredentials(context.Background(), tc.creds), clawLeg(spec, contained))
			if err != nil || got != tc.want {
				t.Errorf("%s (contained=%v) = %+v, %v; want %+v", tc.name, contained, got, err, tc.want)
			}
		}
	}
}

// The stamp's refusal answers for the leaf's sentinel: a reader that asks
// errors.Is(err, credroute.ErrAmbientRefused) matches claw's typed refusal,
// as it will every arm's.
func TestClawsStampRefusalIsTheLeafsAmbientRefusal(t *testing.T) {
	var err error = &credroute.Refusal{Backend: delegate.BackendClaw, Reason: credroute.ReasonAmbientStamp,
		Cause: &ErrAmbientCredentialRefused{Provider: "anthropic", Spec: "anthropic/claude-x"}}
	if !errors.Is(err, credroute.ErrAmbientRefused) {
		t.Fatalf("errors.Is(claw's stamp refusal, credroute.ErrAmbientRefused) = false: %v", err)
	}
	var typed *ErrAmbientCredentialRefused
	if !errors.As(err, &typed) {
		t.Fatalf("the refusal no longer unwraps to the site's type: %v", err)
	}
}

// Every provider claw's registry registers a factory for is walked by the
// container arm by name: the three it walks branch by branch (anthropic,
// openai, the gateway), every other one through the credential its env
// factory reads (clawEnvFactories) — a name the crossing forwards. A provider
// added to registerDefaults without an entry would have the arm refuse a
// node its container serves; an entry with no factory, admit one the
// container refuses.
func TestEveryDefaultProviderNamesItsContainerCredential(t *testing.T) {
	walked := map[string]bool{"anthropic": true, "openai": true, modelroute.OpenAICompatible: true}
	registered := NewSandboxRunnerRegistry(nil).providers
	for name := range registered {
		if _, ok := clawEnvFactories[name]; walked[name] == ok {
			t.Errorf("provider %q: walked by its own branch %v, in clawEnvFactories %v — exactly one reads its container credential", name, walked[name], ok)
		}
	}
	for name, f := range clawEnvFactories {
		if registered[name] == nil {
			t.Errorf("clawEnvFactories names %q, a provider the in-container registry builds no client for", name)
		}
		for _, v := range []string{f.key, f.baseURL} {
			if v != "" && !slices.Contains(providerCredentialEnvVars, v) {
				t.Errorf("provider %q reads %s, a name the crossing never forwards", name, v)
			}
		}
		if f.key == "" && !f.identityChain {
			t.Errorf("provider %q reads no key and serves from no identity chain: nothing can fund it", name)
		}
	}
}

// A spec the in-container registry cannot resolve is refused by the arm as
// the container refuses it, whatever credential the crossing hands over: a
// provider it registers no factory for, and an unreadable spec. Mutant — the
// arm naming the first forwarded credential for them: both admit on the
// pod's key.
func TestTheContainerArmRefusesASpecItsRegistryCannotResolve(t *testing.T) {
	for _, name := range providerCredentialEnvVars {
		t.Setenv(name, "")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "sk-openai-pod")
	t.Setenv("GEMINI_API_KEY", "gemini-pod")
	for _, spec := range []string{"gemini/gemini-2.5-pro", "gpt-5"} {
		for _, contained := range []bool{false, true} {
			_, err := credroute.Resolve(context.Background(), clawLeg(spec, contained))
			var r *credroute.Refusal
			if !errors.As(err, &r) || r.Reason != credroute.ReasonNoCredential {
				t.Errorf("%s (contained=%v) = %v, want the no-credential refusal the registry's own error is", spec, contained, err)
			}
		}
	}
}

// The container arm refuses what the container's own factory refuses of the
// env that crossed, by the factory's own predicate — moonshot's facade
// refusal of a non-kimi id (clawEnvFactory.refuse), the gateway factory's
// refusal of a pair no client is built for (gatewayEnvConfig) — and admits
// what it serves: the verdict never depends on the placement. Each case runs
// the in-container registry over the crossed env, as the runner does, beside
// the arm's prediction. Mutant — containerSpend skipping the factory's
// refusal (or reading the gateway pair without gatewayEnvConfig): the arm
// admits a call the container's factory refuses.
func TestTheContainerArmRefusesWhatTheContainersFactoryRefuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		spec   string
		env    map[string]string
		refuse bool
	}{
		{"a kimi id on the run's moonshot key", "moonshot/kimi-k2", map[string]string{"MOONSHOT_API_KEY": "moonshot-run"}, false},
		{"a non-kimi id on the run's moonshot key", "moonshot/claude-sonnet-4", map[string]string{"MOONSHOT_API_KEY": "moonshot-run"}, true},
		{"a non-kimi id at the operator's own moonshot endpoint", "moonshot/claude-sonnet-4", map[string]string{"MOONSHOT_API_KEY": "moonshot-run", "MOONSHOT_BASE_URL": "https://kimi-proxy.example.com/anthropic"}, false},
		{"a gateway pair", "openai_compatible/m", map[string]string{"OPENAI_COMPATIBLE_API_KEY": "gw-pod", "OPENAI_COMPATIBLE_BASE_URL": "https://llm-gw.example.com/v1"}, false},
		{"a gateway key with no endpoint", "openai_compatible/m", map[string]string{"OPENAI_COMPATIBLE_API_KEY": "gw-pod", "OPENAI_COMPATIBLE_BASE_URL": ""}, true},
		{"a gateway key at an endpoint no client is built for", "openai_compatible/m", map[string]string{"OPENAI_COMPATIBLE_API_KEY": "gw-pod", "OPENAI_COMPATIBLE_BASE_URL": "ftp://llm-gw.example.com"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, name := range providerCredentialEnvVars {
				t.Setenv(name, "")
			}
			facts := crossingFacts{}
			for name, v := range tc.env {
				t.Setenv(name, v)
				if v != "" && envtrust.CredentialShapedName(name) {
					facts.fund(name, credroute.Spend{Channel: credroute.ChannelPodEnv})
				}
			}
			_, factoryErr := NewSandboxRunnerRegistry(nil).Resolve(tc.spec)
			if (factoryErr != nil) != tc.refuse {
				t.Fatalf("inert bench: the container's factory answers %v, the case says refused=%v", factoryErr, tc.refuse)
			}
			_, err := containerSpend(context.Background(), tc.env, facts, tc.spec, "", credroute.SandboxHomeState{})
			if (err != nil) != tc.refuse {
				t.Errorf("the arm's prediction answers %v where the container's factory answers %v — one verdict whatever the placement", err, factoryErr)
			}
		})
	}
}

// The container's own walk names a factory provider's credential by the
// variable clawEnvFactories says that factory reads — the name the launcher's
// prediction reads the crossing's record under — and its destination by the
// table's base-URL variable: one table, two readers, never two spellings
// drifting apart. With no key, a factory serving from the pod's identity
// chain still spends the pod's env, and any other spends nothing.
func TestEnvFactoryCredentialNamesTheTablesVariable(t *testing.T) {
	for provider, f := range clawEnvFactories {
		t.Run(provider, func(t *testing.T) {
			for _, name := range providerCredentialEnvVars {
				t.Setenv(name, "")
			}
			if f.baseURL != "" {
				t.Setenv(f.baseURL, "https://"+provider+".example.com")
			}
			name, spend := envFactoryCredential(context.Background(), provider, "m")
			switch {
			case f.identityChain && (name != "" || spend.Channel != credroute.ChannelPodEnv):
				t.Errorf("keyless: named %q %+v, want the pod's identity chain under no variable", name, spend)
			case !f.identityChain && (name != "" || spend != credroute.Spend{}):
				t.Errorf("keyless: named %q %+v, want nothing spent", name, spend)
			}
			if f.key == "" {
				return
			}
			t.Setenv(f.key, "key-"+provider)
			name, spend = envFactoryCredential(context.Background(), provider, "m")
			if name != f.key || spend.Channel != credroute.ChannelPodEnv {
				t.Errorf("keyed: named %q %+v, want %s on the pod's env", name, spend, f.key)
			}
			if f.baseURL != "" && spend.BaseURL != "https://"+provider+".example.com" {
				t.Errorf("keyed: destination %q, want the value of %s", spend.BaseURL, f.baseURL)
			}
		})
	}
}

// A call a factory serves from the pod's own identity chain — bedrock with no
// AWS key pair, foundry with no AZURE_OPENAI_API_KEY, vertex on ADC: no
// variable carries the credential — spends the pod's env on either
// placement, and every process that names it names one Spend: the
// in-process call, the arm's prediction of the container, and the
// container's own call over the env the crossing composed and its record —
// the keys the runner writes on its steps and its result, which the ledger
// books on nobody, as in process. The factory's client is a stand-in (each
// SDK's NewClient reads its own configuration, which the naming never
// consults); the naming is the runner's real path (inProcessClaw over the
// sandbox runner's registry: resolveFromEnv, envSpend). Every identity-chain
// entry of clawEnvFactories is driven, so a provider joining them is too.
// Mutant — the crossing's rule (crossingRecord.spendOf) naming nothing for a
// walk that took no variable: the container's own call names the zero Spend.
func TestAnIdentityChainCallNamesThePodsEnvOnEitherPlacement(t *testing.T) {
	driven := 0
	for provider, f := range clawEnvFactories {
		if !f.identityChain {
			continue
		}
		driven++
		t.Run(provider, func(t *testing.T) {
			for _, name := range providerCredentialEnvVars {
				t.Setenv(name, "")
			}
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			want := credroute.Spend{Channel: credroute.ChannelPodEnv}
			if f.baseURL != "" {
				want.BaseURL = "https://" + provider + ".example.com"
				t.Setenv(f.baseURL, want.BaseURL)
			}
			spec := provider + "/m"
			standIn := func(reg *Registry) *Registry {
				reg.Register(provider, func(string) (api.APIClient, error) { return &execMockClient{}, nil })
				return reg
			}
			_, inProcess, err := inProcessClaw(context.Background(), standIn(NewRegistry()), clawLeg(spec, false))
			if err != nil || inProcess != want {
				t.Fatalf("in process: %+v, %v; want %+v", inProcess, err, want)
			}
			res, err := resolveClawCall(context.Background(), NewRegistry(), clawLeg(spec, true))
			if err != nil || res.spend != want {
				t.Fatalf("the arm's prediction of the container: %+v, %v; want %+v", res.spend, err, want)
			}
			// The container's process env is the crossed env and nothing
			// else of the pod's.
			for _, name := range providerCredentialEnvVars {
				t.Setenv(name, "")
			}
			for name, v := range res.env {
				t.Setenv(name, v)
			}
			_, own, err := inProcessClaw(context.Background(), standIn(NewSandboxRunnerRegistry(res.facts.funds)), clawLeg(spec, false))
			if err != nil || own != res.spend {
				t.Errorf("the container names its own call %+v, %v; the arm predicts %+v — the keys the runner writes are not what the readers read", own, err, res.spend)
			}
		})
	}
	if driven == 0 {
		t.Fatal("inert bench: clawEnvFactories names no identity-chain provider")
	}
}
