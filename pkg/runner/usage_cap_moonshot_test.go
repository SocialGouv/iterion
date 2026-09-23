package runner

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// Two facades now ride the anthropic wire and BOTH render as
// "facade:<base-url>", so the prefix alone stopped naming a vendor. A run
// holding both keys must charge each refusal to the credential that was
// actually spent: charging a Moonshot wall to the z.ai fingerprint would
// park the healthy key and keep handing out the frozen one — the exact
// inversion runCredKeys exists to prevent.
func TestUsageCapCredKeys_TellsTheTwoFacadesApart(t *testing.T) {
	msg := &queue.RunMessage{TenantID: "team-7"}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys: map[secrets.Provider]string{
			secrets.ProviderZAI:       "zai-token",
			secrets.ProviderMoonshot:  "moonshot-token",
			secrets.ProviderAnthropic: "sk-ant",
		},
		OAuthCredentialFiles: map[string]string{delegate.BackendClaudeCode: "/tmp/oauth"},
		Fingerprints: map[string]string{
			string(secrets.ProviderZAI):       "fp-zai",
			string(secrets.ProviderMoonshot):  "fp-moonshot",
			string(secrets.ProviderAnthropic): "fp-ant",
			delegate.BackendClaudeCode:        "fp-oauth",
		},
	})
	keys := usageCapCredKeys(ctx, msg)
	scope := usagecap.TenantScope("team-7")

	for _, c := range []struct{ source, wantFP string }{
		// The labels come from the delegate's own env builders, so an
		// operator's base-URL override travels with them.
		{"facade:" + secrets.ZAIDefaultBaseURL, "fp-zai"},
		{"facade:" + secrets.MoonshotDefaultBaseURL, "fp-moonshot"},
		{delegate.PiUsageSourceZAI, "fp-zai"},
		{delegate.PiUsageSourceMoonshot, "fp-moonshot"},
		{"anthropic-direct", "fp-ant"},
		{"anthropic-oauth", "fp-oauth"},
		// Default precedence is secrets.AnthropicWireSlotOrder, head first.
		{"", "fp-zai"},
		{"anthropic-env", "fp-zai"},
		// An unrecognised facade label names no slot; a guess there is the
		// mis-charge itself, so it falls to the default instead.
		{"facade:https://some.operator.proxy/anthropic", "fp-zai"},
	} {
		if got := keys.forSource(c.source); got != usagecap.Key(delegate.BackendClaudeCode, scope, c.wantFP) {
			t.Errorf("forSource(%q) = %q, want fingerprint %q", c.source, got, c.wantFP)
		}
	}
}

// A Moonshot-only bundle is the tenant's own, so its readings must open a
// TENANT meter — not the cross-tenant platform one, where what this team
// measured would be read by every other borrower of a key they do not share.
func TestUsageCapCredKeys_MoonshotOnlyBundleIsTenantScoped(t *testing.T) {
	msg := &queue.RunMessage{TenantID: "team-7"}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:      map[secrets.Provider]string{secrets.ProviderMoonshot: "moonshot-token"},
		Fingerprints: map[string]string{string(secrets.ProviderMoonshot): "fp-moonshot"},
	})
	keys := usageCapCredKeys(ctx, msg)
	want := usagecap.Key(delegate.BackendClaudeCode, usagecap.TenantScope("team-7"), "fp-moonshot")
	if got := keys.forSource(""); got != want {
		t.Errorf("forSource(\"\") = %q, want %q", got, want)
	}
}

// The spend ledger reads the same list as the delegate. A route on this wire
// must name the slot the delegate would actually have spent, or the run is
// billed against a credential it never touched.
func TestCredentialSlotForRoute_FollowsTheWireSlotOrder(t *testing.T) {
	creds := secrets.Credentials{
		APIKeys: map[secrets.Provider]string{
			secrets.ProviderMoonshot:  "moonshot-token",
			secrets.ProviderAnthropic: "sk-ant",
		},
	}
	if got := credentialSlotForRoute(creds, delegate.BackendClaudeCode, ""); got != string(secrets.ProviderMoonshot) {
		t.Errorf("credentialSlotForRoute = %q, want moonshot (earlier than anthropic in secrets.AnthropicWireSlotOrder)", got)
	}
	// A `moonshot/` model spec pins the provider outright: the slot IS the
	// one the spec names, held or not.
	if got := credentialSlotForRoute(creds, "claw", "moonshot/kimi-k2"); got != string(secrets.ProviderMoonshot) {
		t.Errorf("credentialSlotForRoute(moonshot/kimi-k2) = %q, want moonshot", got)
	}
	if got := credentialSlotForRoute(secrets.Credentials{}, "claw", "moonshot/kimi-k2"); got != "" {
		t.Errorf("credentialSlotForRoute with no moonshot key = %q, want \"\" — charge nobody rather than a default", got)
	}
}
