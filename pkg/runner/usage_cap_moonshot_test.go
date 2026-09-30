package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
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
		// The pod's inherited env is no bundle credential.
		{"anthropic-env", ""},
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
	for _, source := range []string{"facade:https://some.operator.proxy/anthropic", ""} {
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
	for _, source := range []string{"", "facade:https://some.operator.proxy/anthropic"} {
		if got, want := keys.forSource(source), usagecap.Key(delegate.BackendClaudeCode, scope, "fp-anthropic"); got != want {
			t.Errorf("forSource(%q) = %q, want the run's own credential %q — an unattributable reading cannot have been spent on a pinned key", source, got, want)
		}
	}
}

// Rf1768e: a pinned key belongs to a shared tier, so ITS readings belong to
// that tier's ledger — even when the RUN is tenant-scoped because the tenant
// also holds a credential on the same wire (which is always the case: a
// pinned key exists only beside another one). Metering it on the tenant's
// private ledger would hide a window refusal from every other borrower of the
// same platform account.
//
// Both directions on one bench: the tenant's own credential keeps the tenant
// scope, the pinned one gets the platform's.
func TestUsageCapCredKeys_APinnedSlotMetersOnItsOwnersLedger(t *testing.T) {
	msg := &queue.RunMessage{TenantID: "team-7", OrgID: "org-1"}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:         map[secrets.Provider]string{secrets.ProviderAnthropic: "tenant-anthropic"},
		PinnedAPIKeys:   map[secrets.Provider]string{secrets.ProviderMoonshot: "platform-moonshot"},
		PlatformSourced: map[string]bool{string(secrets.ProviderMoonshot): true},
		Fingerprints: map[string]string{
			string(secrets.ProviderAnthropic): "fp-anthropic",
			string(secrets.ProviderMoonshot):  "fp-moonshot",
		},
	})
	keys := usageCapCredKeys(ctx, msg)
	tenant := usagecap.TenantScope("team-7")

	// The run is tenant-scoped: its own key decides that.
	if got, want := keys.forSource("anthropic-direct"), usagecap.Key(delegate.BackendClaudeCode, tenant, "fp-anthropic"); got != want {
		t.Errorf("forSource(anthropic-direct) = %q, want %q", got, want)
	}
	// …and the shared pinned key is metered on the platform's ledger, not on
	// that private one.
	moonshot := facadeSource(secrets.ProviderMoonshot, secrets.MoonshotDefaultBaseURL)
	if got, want := keys.forSource(moonshot), usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, "fp-moonshot"); got != want {
		t.Errorf("forSource(%q) = %q, want %q — a shared key on a tenant's private ledger hides its walls from every other borrower", moonshot, got, want)
	}
}

// The org's own pinned key meters on the ORG's ledger: its subscription
// serves every team of its audience, and merging it with the platform's would
// make one org's exhausted window park every other tenant's runs.
func TestUsageCapCredKeys_AnOrgPinnedSlotMetersOnTheOrgLedger(t *testing.T) {
	msg := &queue.RunMessage{TenantID: "team-7", OrgID: "org-1"}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:       map[secrets.Provider]string{secrets.ProviderAnthropic: "tenant-anthropic"},
		PinnedAPIKeys: map[secrets.Provider]string{secrets.ProviderMoonshot: "org-moonshot"},
		OrgSourced:    map[string]bool{string(secrets.ProviderMoonshot): true},
		Fingerprints: map[string]string{
			string(secrets.ProviderAnthropic): "fp-anthropic",
			string(secrets.ProviderMoonshot):  "fp-moonshot",
		},
	})
	keys := usageCapCredKeys(ctx, msg)
	moonshot := facadeSource(secrets.ProviderMoonshot, secrets.MoonshotDefaultBaseURL)
	if got, want := keys.forSource(moonshot), usagecap.Key(delegate.BackendClaudeCode, usagecap.OrgScope("org-1"), "fp-moonshot"); got != want {
		t.Errorf("forSource(%q) = %q, want %q", moonshot, got, want)
	}
}

// The incident shape: the run's anthropic wire is held by a platform z.ai
// key, an `anthropic`-pinned node finds no Anthropic credential in the bundle
// and inherits the pod's env, and the pod has no ambient auth — its "Not
// logged in" is recorded as an auth refusal. That refusal belongs to no
// bundle credential; charged to the head of the precedence, it would bench
// the z.ai key the whole platform shares.
func TestUsageCapCredKeys_AmbientEnvChargesNoBundleCredential(t *testing.T) {
	msg := &queue.RunMessage{TenantID: "team-7"}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:         map[secrets.Provider]string{secrets.ProviderZAI: "zai-platform"},
		PlatformSourced: map[string]bool{string(secrets.ProviderZAI): true},
		Fingerprints:    map[string]string{string(secrets.ProviderZAI): "fp-zai"},
	})
	keys := usageCapCredKeys(ctx, msg)
	if got, bench := keys.forSource("anthropic-env"), usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, "fp-zai"); got == bench {
		t.Errorf("forSource(anthropic-env) = %q — an ambient-env refusal was charged to the z.ai key the session never used", got)
	}
	if got, want := keys.forSource("anthropic-env"), usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, ""); got != want {
		t.Errorf("forSource(anthropic-env) = %q, want the credential-less meter %q", got, want)
	}
	// The run's default still meters on the key: the pre-flight reads it.
	if got, want := keys.forSource(""), usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, "fp-zai"); got != want {
		t.Errorf("forSource(\"\") = %q, want the bundle default %q", got, want)
	}
}

// A GLM route spends the z.ai key — the default one or, beside a forfait
// holding the wire, the key a shared tier pinned for it — and its spend is
// booked there. The head of the wire's precedence is the forfait, which never
// served it.
func TestCredentialSlotForRoute_GLMIsBookedOnTheZAIKey(t *testing.T) {
	beside := secrets.Credentials{
		PinnedAPIKeys:        map[secrets.Provider]string{secrets.ProviderZAI: "zai-pinned"},
		OAuthCredentialFiles: map[string]string{delegate.BackendClaudeCode: "/forfait"},
	}
	for _, r := range []struct{ backend, model string }{
		{delegate.BackendClaudeCode, "glm-5.3"},
		{"claw", "anthropic/glm-5.3"},
		{delegate.BackendPi, "glm-5.3"},
	} {
		if got := credentialSlotForRoute(beside, r.backend, r.model); got != string(secrets.ProviderZAI) {
			t.Errorf("credentialSlotForRoute(%s, %s) = %q, want zai — a metered key's spend was booked elsewhere", r.backend, r.model, got)
		}
	}
	// The forfait's own route is still the forfait's.
	if got := credentialSlotForRoute(beside, delegate.BackendClaudeCode, "claude-opus-5-5"); got != string(secrets.OAuthKindClaudeCode) {
		t.Errorf("credentialSlotForRoute(claude-opus-5-5) = %q, want the forfait", got)
	}
	// No z.ai key at all: charge nobody, never the forfait.
	forfaitOnly := secrets.Credentials{OAuthCredentialFiles: map[string]string{delegate.BackendClaudeCode: "/forfait"}}
	if got := credentialSlotForRoute(forfaitOnly, delegate.BackendClaudeCode, "glm-5.3"); got != "" {
		t.Errorf("credentialSlotForRoute(glm-5.3) with no z.ai key = %q, want \"\"", got)
	}
	// A GLM another provider serves is that provider's.
	router := secrets.Credentials{APIKeys: map[secrets.Provider]string{secrets.ProviderOpenRouter: "or-key", secrets.ProviderZAI: "zai-key"}}
	if got := credentialSlotForRoute(router, "claw", "openrouter/z-ai/glm-4.6"); got != string(secrets.ProviderOpenRouter) {
		t.Errorf("credentialSlotForRoute(openrouter/z-ai/glm-4.6) = %q, want openrouter", got)
	}
}

// A spec that names its provider spends the key a shared tier pinned for it
// (claw forwards exactly that key for exactly that node), so its spend is
// booked on it rather than on nobody.
func TestCredentialSlotForRoute_APrefixPinIsBookedOnItsPinnedKey(t *testing.T) {
	creds := secrets.Credentials{
		APIKeys:       map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-tenant"},
		PinnedAPIKeys: map[secrets.Provider]string{secrets.ProviderMoonshot: "moonshot-platform"},
	}
	if got := credentialSlotForRoute(creds, "claw", "moonshot/kimi-k2"); got != string(secrets.ProviderMoonshot) {
		t.Errorf("credentialSlotForRoute(moonshot/kimi-k2) = %q, want moonshot — the pinned key it spent", got)
	}
}

// A claw `anthropic/…` node spends the Anthropic key the run holds for its
// route — here one a shared tier pinned beside the tenant's own z.ai key — and
// its spend is booked there, not on the head of the wire's precedence.
func TestCredentialSlotForRoute_AClawAnthropicSpecIsBookedOnItsKey(t *testing.T) {
	creds := secrets.Credentials{
		APIKeys:       map[secrets.Provider]string{secrets.ProviderZAI: "zai-tenant"},
		PinnedAPIKeys: map[secrets.Provider]string{secrets.ProviderAnthropic: "anthropic-platform"},
	}
	if got := credentialSlotForRoute(creds, "claw", "anthropic/claude-opus-5-5"); got != string(secrets.ProviderAnthropic) {
		t.Errorf("credentialSlotForRoute(claw, anthropic/claude-opus-5-5) = %q, want anthropic — the key it spent", got)
	}
	// claude_code carries its pin as a hint the route key does not hold: the
	// wire's default precedence answers there.
	if got := credentialSlotForRoute(creds, delegate.BackendClaudeCode, "claude-opus-5-5"); got != string(secrets.ProviderZAI) {
		t.Errorf("credentialSlotForRoute(claude_code, claude-opus-5-5) = %q, want the default precedence (zai)", got)
	}
}

// A claw `openai/…` node spends the run's default openai key, then its ChatGPT
// forfait, then a key a shared tier pinned for the route — the order
// model.ResolveWithContext applies — and its spend is booked on what it spent.
// codex never reads a pinned key.
func TestCredentialSlotForRoute_AClawOpenAISpecFollowsClawsOrder(t *testing.T) {
	codex := map[string]string{string(secrets.OAuthKindCodex): chatGPTForfaitDir(t)}
	pinned := map[secrets.Provider]string{secrets.ProviderOpenAI: "openai-platform"}
	for _, tc := range []struct {
		name    string
		creds   secrets.Credentials
		backend string
		want    string
	}{
		{"the forfait before the pinned key", secrets.Credentials{PinnedAPIKeys: pinned, OAuthCredentialFiles: codex}, "claw", string(secrets.OAuthKindCodex)},
		{"the run's own key first", secrets.Credentials{APIKeys: map[secrets.Provider]string{secrets.ProviderOpenAI: "sk-tenant"}, PinnedAPIKeys: pinned, OAuthCredentialFiles: codex}, "claw", string(secrets.ProviderOpenAI)},
		{"the pinned key when nothing else", secrets.Credentials{PinnedAPIKeys: pinned}, "claw", string(secrets.ProviderOpenAI)},
		{"codex never spends a pinned key", secrets.Credentials{PinnedAPIKeys: pinned}, delegate.BackendCodex, ""},
	} {
		if got := credentialSlotForRoute(tc.creds, tc.backend, "openai/gpt-6"); got != tc.want {
			t.Errorf("%s: credentialSlotForRoute(%s, openai/gpt-6) = %q, want %q", tc.name, tc.backend, got, tc.want)
		}
	}
}

// The ledger books a claw `openai/…` route on the credential claw actually
// spent — the in-process registry's own choice, asked through the lookups a
// runner installs — for every combination of the run's key, a pinned key, a
// ChatGPT forfait and this runner's OAuth settings.
func TestCredentialSlotForRoute_AClawOpenAIRouteIsBookedWhereClawSpent(t *testing.T) {
	model.SetCredentialsLookup(model.RunCredentialsLookup)
	model.SetOAuthDirLookup(model.RunOAuthDirLookup)
	t.Cleanup(func() {
		model.SetCredentialsLookup(func(context.Context) (func(string) string, bool) { return nil, false })
		model.SetOAuthDirLookup(func(context.Context) (func(string) string, bool) { return nil, false })
	})
	blob := t.TempDir()
	if err := os.WriteFile(filepath.Join(blob, "auth.json"), []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"tok-abc","account_id":"acct-xyz"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, defKey := range []bool{false, true} {
		for _, pinned := range []bool{false, true} {
			for _, refuse := range []bool{false, true} {
				for _, gateway := range []bool{false, true} {
					t.Run(fmt.Sprintf("default=%v pinned=%v refuse=%v gateway=%v", defKey, pinned, refuse, gateway), func(t *testing.T) {
						t.Setenv("OPENAI_API_KEY", "")
						t.Setenv("CODEX_HOME", t.TempDir())
						t.Setenv("ITERION_OPENAI_USE_OAUTH", map[bool]string{true: "0", false: ""}[refuse])
						t.Setenv("OPENAI_BASE_URL", map[bool]string{true: "http://gateway.ledger.example/v1", false: ""}[gateway])
						creds := secrets.Credentials{
							APIKeys:              map[secrets.Provider]string{},
							PinnedAPIKeys:        map[secrets.Provider]string{},
							OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindCodex): blob},
						}
						if defKey {
							creds.APIKeys[secrets.ProviderOpenAI] = "sk-default"
						}
						if pinned {
							creds.PinnedAPIKeys[secrets.ProviderOpenAI] = "sk-pinned"
						}
						client, err := model.NewRegistry().ResolveWithContext(secrets.WithCredentials(context.Background(), creds), "openai/gpt-6")
						spent := ""
						if err == nil {
							v := reflect.ValueOf(client).Elem()
							switch {
							case v.FieldByName("OAuthToken").String() != "":
								spent = string(secrets.OAuthKindCodex)
							case v.FieldByName("APIKey").String() != "":
								spent = string(secrets.ProviderOpenAI)
							}
						}
						if got := credentialSlotForRoute(creds, delegate.BackendClaw, "openai/gpt-6"); got != spent {
							t.Errorf("booked on %q, claw spent %q", got, spent)
						}
					})
				}
			}
		}
	}
}

// The run's scope is its DEFAULT credentials': beside an org's pinned z.ai
// key, the platform's forfait holding the wire is metered on the platform's
// ledger — the one the publisher's window check and every other team's
// pre-flight read — and the pinned key keeps its owner's. A run holding pinned
// keys alone takes its scope from them.
func TestUsageCapCredKeys_APinnedKeyDoesNotScopeTheDefaultCredential(t *testing.T) {
	msg := &queue.RunMessage{TenantID: "team-7", OrgID: "org-1"}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		PinnedAPIKeys:        map[secrets.Provider]string{secrets.ProviderZAI: "org-zai"},
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): "/forfait"},
		OrgSourced:           map[string]bool{string(secrets.ProviderZAI): true},
		PlatformSourced:      map[string]bool{string(secrets.OAuthKindClaudeCode): true},
		Fingerprints: map[string]string{
			string(secrets.OAuthKindClaudeCode): "fp-platform-forfait",
			string(secrets.ProviderZAI):         "fp-org-zai",
		},
	})
	keys := usageCapCredKeys(ctx, msg)
	if got, want := keys.forSource("anthropic-oauth"), usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, "fp-platform-forfait"); got != want {
		t.Errorf("forSource(anthropic-oauth) = %q, want %q — the platform's forfait metered on the org's ledger", got, want)
	}
	zai := facadeSource(secrets.ProviderZAI, secrets.ZAIDefaultBaseURL)
	if got, want := keys.forSource(zai), usagecap.Key(delegate.BackendClaudeCode, usagecap.OrgScope("org-1"), "fp-org-zai"); got != want {
		t.Errorf("forSource(%q) = %q, want %q", zai, got, want)
	}

	pinnedOnly := secrets.WithCredentials(context.Background(), secrets.Credentials{
		PinnedAPIKeys: map[secrets.Provider]string{secrets.ProviderZAI: "org-zai"},
		OrgSourced:    map[string]bool{string(secrets.ProviderZAI): true},
		Fingerprints:  map[string]string{string(secrets.ProviderZAI): "fp-org-zai"},
	})
	if got, want := usageCapKey(pinnedOnly, msg), usagecap.Key(delegate.BackendClaudeCode, usagecap.OrgScope("org-1"), ""); got != want {
		t.Errorf("a run holding only an org's pinned key scoped %q, want %q", got, want)
	}
}

// Each backend's spend is booked in ITS order, not in claude_code's: pi reads
// only the key of the provider its spec names — the run's default one, then one
// a shared tier pinned for the route — never a forfait nor a facade; kimi, grok
// and opencode read their own config, so iterion supplied nothing they spent;
// claw's anthropic provider never reads a Moonshot key.
func TestCredentialSlotForRoute_EachBackendIsBookedInItsOwnOrder(t *testing.T) {
	forfaits := map[string]string{
		string(secrets.OAuthKindClaudeCode): "/forfait",
		string(secrets.OAuthKindCodex):      chatGPTForfaitDir(t),
	}
	for _, tc := range []struct {
		name           string
		creds          secrets.Credentials
		backend, model string
		want           string
	}{
		{"pi on anthropic spends the key pinned for it, not the forfait nor the z.ai default",
			secrets.Credentials{APIKeys: map[secrets.Provider]string{secrets.ProviderZAI: "zai-tenant"}, PinnedAPIKeys: map[secrets.Provider]string{secrets.ProviderAnthropic: "ant-platform"}, OAuthCredentialFiles: forfaits},
			delegate.BackendPi, "anthropic/claude-opus-5-5", string(secrets.ProviderAnthropic)},
		{"pi on anthropic with no Anthropic key spends nothing iterion supplied",
			secrets.Credentials{APIKeys: map[secrets.Provider]string{secrets.ProviderZAI: "zai-tenant"}, OAuthCredentialFiles: forfaits},
			delegate.BackendPi, "anthropic/claude-opus-5-5", ""},
		{"pi on openai spends the key pinned for it, not the ChatGPT forfait",
			secrets.Credentials{PinnedAPIKeys: map[secrets.Provider]string{secrets.ProviderOpenAI: "openai-platform"}, OAuthCredentialFiles: forfaits},
			delegate.BackendPi, "openai/gpt-6", string(secrets.ProviderOpenAI)},
		{"pi on openai with no OpenAI key spends nothing iterion supplied",
			secrets.Credentials{OAuthCredentialFiles: forfaits},
			delegate.BackendPi, "openai/gpt-6", ""},
		{"opencode spends its own config",
			secrets.Credentials{APIKeys: map[secrets.Provider]string{secrets.ProviderAnthropic: "ant-tenant"}, OAuthCredentialFiles: forfaits},
			delegate.BackendOpenCode, "anthropic/claude-opus-5-5", ""},
		{"kimi spends its own config",
			secrets.Credentials{APIKeys: map[secrets.Provider]string{secrets.ProviderMoonshot: "moonshot-tenant"}},
			delegate.BackendKimi, "moonshot/kimi-k2", ""},
		{"grok spends its own config",
			secrets.Credentials{APIKeys: map[secrets.Provider]string{secrets.ProviderXAI: "xai-tenant"}},
			delegate.BackendGrok, "xai/grok-4", ""},
		{"claw's anthropic provider never reads a Moonshot key",
			secrets.Credentials{APIKeys: map[secrets.Provider]string{secrets.ProviderMoonshot: "moonshot-tenant"}, OAuthCredentialFiles: forfaits},
			delegate.BackendClaw, "anthropic/claude-opus-5-5", string(secrets.OAuthKindClaudeCode)},
	} {
		if got := credentialSlotForRoute(tc.creds, tc.backend, tc.model); got != tc.want {
			t.Errorf("%s: credentialSlotForRoute(%s, %s) = %q, want %q", tc.name, tc.backend, tc.model, got, tc.want)
		}
	}
}
