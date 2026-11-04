package model

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
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
