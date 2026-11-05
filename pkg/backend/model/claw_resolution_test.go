package model

import (
	"context"
	"errors"
	"slices"
	"testing"

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
	registered := NewSandboxRunnerRegistry().providers
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
