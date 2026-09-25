package runner

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// facadeSource renders the routing label the delegate stamps on a reading
// served through an anthropic-wire facade. Spelled here rather than imported
// because it is the WIRE SHAPE this package reads: if the delegate ever
// renders another, the rows below are what says so.
func facadeSource(slot secrets.Provider, baseURL string) string {
	return "facade:" + string(slot) + ":" + baseURL
}

// Two facades now ride the anthropic wire and BOTH render as
// "facade:…<base-url>", so the URL alone cannot name a vendor. A run
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
		// The label names the SLOT that paid, stamped by the delegate that
		// built the env (delegate.FacadeSlotEnvKey): the base URL rides along
		// for the operator to read, and two facades may legitimately share it.
		{facadeSource(secrets.ProviderZAI, secrets.ZAIDefaultBaseURL), "fp-zai"},
		{facadeSource(secrets.ProviderMoonshot, secrets.MoonshotDefaultBaseURL), "fp-moonshot"},
		// Both vendors behind one gateway: same URL, still two credentials.
		{facadeSource(secrets.ProviderZAI, "https://gateway.internal/anthropic"), "fp-zai"},
		{facadeSource(secrets.ProviderMoonshot, "https://gateway.internal/anthropic"), "fp-moonshot"},
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

// A slot the label NAMES but the run does not CARRY must charge nobody.
//
// It is a reachable state, not a curiosity: a pod-level MOONSHOT_API_KEY funds
// a moonshot-pinned node (delegate.facadeCredEnvForHint reads it) and is not a
// BYOK record, so the run holds no moonshot fingerprint at all while its
// readings still say moonshot. Falling back to the head of the precedence
// charges that Moonshot wall to the z.ai key — the meter then parks the
// healthy credential and keeps handing out the walled one, which is the
// inversion runCredKeys exists to prevent. The comment on bySlot already says
// a guess would be that fault for an UNKNOWN label; it is the same fault here,
// with the slot known.
//
// The four-fingerprint bench above cannot see this: it sows every slot, so
// bySlot always answers.
func TestUsageCapCredKeys_NamedButUnheldSlotChargesNobody(t *testing.T) {
	msg := &queue.RunMessage{TenantID: "team-7"}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:      map[secrets.Provider]string{secrets.ProviderZAI: "zai-token"},
		Fingerprints: map[string]string{string(secrets.ProviderZAI): "fp-zai"},
	})
	keys := usageCapCredKeys(ctx, msg)
	scope := usagecap.TenantScope("team-7")

	moonshot := facadeSource(secrets.ProviderMoonshot, secrets.MoonshotDefaultBaseURL)
	// The label must be one the delegate actually names a slot for, or this
	// bench is measuring its own spelling rather than the meter.
	if got := delegate.AnthropicWireFacadeSlot(moonshot); got != string(secrets.ProviderMoonshot) {
		t.Fatalf("inert bench: AnthropicWireFacadeSlot(%q) = %q, want moonshot", moonshot, got)
	}
	if got, want := keys.forSource(moonshot), usagecap.Key(delegate.BackendClaudeCode, scope, ""); got != want {
		t.Errorf("forSource(%q) = %q, want %q — a named slot the run does not hold charges nobody", moonshot, got, want)
	}

	// The guard discriminates: the slot the run DOES hold is still charged,
	// and a label naming no slot keeps falling to the bundle default (there,
	// the default is the only answer available).
	zai := facadeSource(secrets.ProviderZAI, secrets.ZAIDefaultBaseURL)
	if got, want := keys.forSource(zai), usagecap.Key(delegate.BackendClaudeCode, scope, "fp-zai"); got != want {
		t.Errorf("forSource(%q) = %q, want %q", zai, got, want)
	}
	for _, source := range []string{"facade:https://some.operator.proxy/anthropic", "", "anthropic-env"} {
		if got, want := keys.forSource(source), usagecap.Key(delegate.BackendClaudeCode, scope, "fp-zai"); got != want {
			t.Errorf("forSource(%q) = %q, want the bundle default %q", source, got, want)
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

// A slot funded ONLY by a pinned key (secrets.Credentials.PinnedAPIKeys) is
// charged for ITS OWN readings — that is why its fingerprint is stamped at
// all — but it is not the run's default credential: no unpinned node can
// spend it, so a reading with no attributable source must not land there.
// Charging it would park a key this run's unpinned work never touched.
//
// Both directions, one bench: the named label reaches it, the unattributable
// one does not.
func TestUsageCapCredKeys_APinnedSlotIsNotTheBundleDefault(t *testing.T) {
	msg := &queue.RunMessage{TenantID: "team-7"}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		// The run's own instrument is the Anthropic key; moonshot is funded
		// only because a route pins it.
		APIKeys:       map[secrets.Provider]string{secrets.ProviderAnthropic: "anthropic-key"},
		PinnedAPIKeys: map[secrets.Provider]string{secrets.ProviderMoonshot: "platform-moonshot"},
		Fingerprints: map[string]string{
			string(secrets.ProviderAnthropic): "fp-anthropic",
			string(secrets.ProviderMoonshot):  "fp-moonshot",
		},
	})
	keys := usageCapCredKeys(ctx, msg)
	scope := usagecap.TenantScope("team-7")

	moonshot := facadeSource(secrets.ProviderMoonshot, secrets.MoonshotDefaultBaseURL)
	if got := delegate.AnthropicWireFacadeSlot(moonshot); got != string(secrets.ProviderMoonshot) {
		t.Fatalf("inert bench: AnthropicWireFacadeSlot(%q) = %q, want moonshot", moonshot, got)
	}
	// Its own readings: charged to it, or a Moonshot wall would be invisible
	// and the walled key would keep being handed to the pinned node.
	if got, want := keys.forSource(moonshot), usagecap.Key(delegate.BackendClaudeCode, scope, "fp-moonshot"); got != want {
		t.Errorf("forSource(%q) = %q, want %q — a pinned key must be charged for what IT spent", moonshot, got, want)
	}
	// The bundle default: never the pinned slot, even though the wire order
	// puts moonshot first.
	for _, source := range []string{"", "anthropic-env", "facade:https://some.operator.proxy/anthropic"} {
		if got, want := keys.forSource(source), usagecap.Key(delegate.BackendClaudeCode, scope, "fp-anthropic"); got != want {
			t.Errorf("forSource(%q) = %q, want the run's own credential %q — an unattributable reading cannot have been spent on a pinned key", source, got, want)
		}
	}
}
