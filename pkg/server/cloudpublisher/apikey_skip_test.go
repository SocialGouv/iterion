package cloudpublisher

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// The BYOK walk's evidence predicate, both faces: only a fresh quota-family
// refusal under the KEY'S OWN fingerprint skips it. Everything uncertain —
// no reading, another provider's meter, the pay-as-you-go overage channel —
// keeps the key usable, because a wrong skip silently moves spend onto a
// credential nobody chose.
func TestApiKeyUsable(t *testing.T) {
	st := usagecap.NewMemStore()
	p := &Publisher{usageCaps: st, logger: iterlog.New(iterlog.LevelError, nil)}
	scope := usagecap.TenantScope("team")
	key := secrets.ApiKey{Provider: secrets.ProviderZAI, Name: "primary", Fingerprint: "fp-zai-1"}

	usable := p.apiKeyUsable(context.Background(), scope, "run-x")
	if !usable(key) {
		t.Fatal("no evidence must mean usable")
	}

	// The provider refused this key's account rate (the frequency window
	// #610 records) — the walk must pass it over.
	if err := st.Record(context.Background(),
		usagecap.Key(delegate.BackendClaudeCode, scope, "fp-zai-1"),
		usagecap.Reading{Window: usagecap.WindowFrequency, Status: usagecap.StatusRejected,
			ObservedAt: time.Now()}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if usable(key) {
		t.Fatal("a fresh frequency refusal under the key's fingerprint must skip it")
	}

	// Hysteric guards: a rejected OVERAGE reading is money, not quota; and
	// a provider with no metered backend has no evidence to act on.
	st2 := usagecap.NewMemStore()
	p2 := &Publisher{usageCaps: st2, logger: iterlog.New(iterlog.LevelError, nil)}
	if err := st2.Record(context.Background(),
		usagecap.Key(delegate.BackendClaudeCode, scope, "fp-zai-1"),
		usagecap.Reading{Window: usagecap.WindowOverage, Status: usagecap.StatusRejected,
			ObservedAt: time.Now()}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if !p2.apiKeyUsable(context.Background(), scope, "run-x")(key) {
		t.Fatal("a rejected overage reading is no quota evidence — the key must stay usable")
	}
	other := secrets.ApiKey{Provider: secrets.ProviderOpenAI, Name: "m", Fingerprint: "fp-zai-1"}
	if !usable(other) {
		t.Fatal("a provider with no metered backend must never be skipped")
	}
}

// seedKeyFP seeds a sealed API key carrying an explicit fingerprint — the
// identity the evidence predicate matches refusals against.
func seedKeyFP(t *testing.T, st secrets.ApiKeyStore, sealer secrets.Sealer, teamID string, provider secrets.Provider, plaintext, fp string) {
	t.Helper()
	id := secrets.NewApiKeyID()
	sealed, err := secrets.SealAPIKey(sealer, id, []byte(plaintext))
	if err != nil {
		t.Fatalf("seal key: %v", err)
	}
	if err := st.Create(context.Background(), secrets.ApiKey{
		ID: id, ScopeTeamID: teamID, Provider: provider, Fingerprint: fp,
		Name: string(provider) + "-" + fp, SealedSecret: sealed, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}
}

func recordRefusal(t *testing.T, st usagecap.Store, scope, fp string) {
	t.Helper()
	if err := st.Record(context.Background(),
		usagecap.Key(delegate.BackendClaudeCode, scope, fp),
		usagecap.Reading{Window: usagecap.WindowFrequency, Status: usagecap.StatusRejected,
			ObservedAt: time.Now()}); err != nil {
		t.Fatalf("record refusal: %v", err)
	}
}

// A provider whose ONLY key is refused must still fund the run when no
// other tier can serve its wire: the run then makes one refused call and
// parks on a durable usage-window retry, instead of being published with
// an empty wire that fails on a no-credential auth error nothing retries.
func TestApiKeySkip_refusedOnlyKeyIsRestored(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKeyFP(t, keys, sealer, "team1", secrets.ProviderAnthropic, "sk-only", "fp-a")
	caps := usagecap.NewMemStore()
	recordRefusal(t, caps, usagecap.TenantScope("team1"), "fp-a")

	p := &Publisher{apiKeys: keys, usageCaps: caps,
		runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer, logger: testLogger()}
	rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)

	b := resolveBundle(t, p, rs, sealer, "run-r1", "team1", "owner1")
	if got := b.APIKeys[secrets.ProviderAnthropic]; got != "sk-only" {
		t.Fatalf("anthropic key = %q, want the refused key RESTORED — an empty wire is a stuck run", got)
	}
	if b.PlatformSourced[string(secrets.ProviderAnthropic)] {
		t.Fatal("a restored TENANT key must not be marked platform-sourced")
	}
}

// With a second key of the same provider, the walk serves it and the
// refused one stays out — restore is strictly the no-other-option path.
func TestApiKeySkip_secondKeyServesNoRestore(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKeyFP(t, keys, sealer, "team1", secrets.ProviderAnthropic, "sk-frozen", "fp-a")
	seedKeyFP(t, keys, sealer, "team1", secrets.ProviderAnthropic, "sk-healthy", "fp-b")
	caps := usagecap.NewMemStore()
	recordRefusal(t, caps, usagecap.TenantScope("team1"), "fp-a")

	p := &Publisher{apiKeys: keys, usageCaps: caps,
		runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer, logger: testLogger()}
	rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)

	b := resolveBundle(t, p, rs, sealer, "run-r2", "team1", "owner1")
	if got := b.APIKeys[secrets.ProviderAnthropic]; got != "sk-healthy" {
		t.Fatalf("anthropic key = %q, want the healthy second key", got)
	}
}

// The refused tenant key must not block the fall-through: a healthy
// platform key on the same wire serves the run — the manual key-removal
// this PR retires. The tenant key is NOT restored over it.
func TestApiKeySkip_refusedTenantKeyFallsThroughToPlatform(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKeyFP(t, keys, sealer, "team1", secrets.ProviderAnthropic, "sk-frozen", "fp-a")
	seedKeyFP(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderAnthropic, "sk-platform", "fp-p")
	caps := usagecap.NewMemStore()
	recordRefusal(t, caps, usagecap.TenantScope("team1"), "fp-a")

	p := &Publisher{apiKeys: keys, usageCaps: caps,
		runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer, logger: testLogger()}
	rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)

	b := resolveBundle(t, p, rs, sealer, "run-r3", "team1", "owner1")
	if got := b.APIKeys[secrets.ProviderAnthropic]; got != "sk-platform" {
		t.Fatalf("anthropic key = %q, want the platform key — the fallback chain must engage", got)
	}
	if !b.PlatformSourced[string(secrets.ProviderAnthropic)] {
		t.Fatal("the platform fill must keep its shared-meter scope")
	}
}

// The platform tier itself: its refused-but-only key is restored WITH its
// platform metering scope — the last DB-backed tier is never left empty,
// the rule its OAuth sibling states.
func TestApiKeySkip_refusedPlatformKeyIsRestoredPlatformSourced(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKeyFP(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderZAI, "sk-zai-platform", "fp-z")
	caps := usagecap.NewMemStore()
	recordRefusal(t, caps, usagecap.ScopePlatform, "fp-z")

	p := &Publisher{apiKeys: keys, usageCaps: caps,
		runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer, logger: testLogger()}
	rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)

	b := resolveBundle(t, p, rs, sealer, "run-r4", "team1", "owner1")
	if got := b.APIKeys[secrets.ProviderZAI]; got != "sk-zai-platform" {
		t.Fatalf("zai key = %q, want the refused platform key RESTORED", got)
	}
	if !b.PlatformSourced[string(secrets.ProviderZAI)] {
		t.Fatal("a restored PLATFORM key must keep its platform-sourced metering scope")
	}
}

// The third evidence family: a provider that rejected the CREDENTIAL
// ITSELF (dead token, malformed secret) must be walked past exactly like
// a quota refusal — without this, a structurally-broken credential keeps
// filling its slot on every re-resolution and gates the healthy tiers off.
func TestApiKeyUsable_authRefusalSkips(t *testing.T) {
	st := usagecap.NewMemStore()
	p := &Publisher{usageCaps: st, logger: iterlog.New(iterlog.LevelError, nil)}
	scope := usagecap.TenantScope("team")
	key := secrets.ApiKey{Provider: secrets.ProviderAnthropic, Name: "dead", Fingerprint: "fp-dead"}
	if err := st.Record(context.Background(),
		usagecap.Key(delegate.BackendClaudeCode, scope, "fp-dead"),
		usagecap.Reading{Window: usagecap.WindowAuth, Status: usagecap.StatusRejected,
			ObservedAt: time.Now()}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if p.apiKeyUsable(context.Background(), scope, "run-x")(key) {
		t.Fatal("a fresh auth refusal under the key's fingerprint must skip it")
	}
}
