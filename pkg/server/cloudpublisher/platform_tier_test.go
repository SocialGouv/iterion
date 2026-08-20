package cloudpublisher

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/credpool"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The platform tier: the deployment's own DB-backed credentials, filling
// the slots the tenant tiers and the pool left empty — the rotate-without-
// redeploy replacement for the runner-pod env fallback.

// seedPlatformKey seals an API key row under the given team scope (the
// sentinel secrets.PlatformTenantID for platform keys, a real team id for
// tenant BYOK).
func seedKey(t *testing.T, st secrets.ApiKeyStore, sealer secrets.Sealer, teamID string, provider secrets.Provider, plaintext string) {
	t.Helper()
	id := secrets.NewApiKeyID()
	sealed, err := secrets.SealAPIKey(sealer, id, []byte(plaintext))
	if err != nil {
		t.Fatalf("seal key: %v", err)
	}
	if err := st.Create(context.Background(), secrets.ApiKey{
		ID: id, ScopeTeamID: teamID, Provider: provider,
		Name: string(provider), SealedSecret: sealed, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}
}

// A run that resolved nothing anywhere gets the platform credentials —
// one per wire family — and the bundle records which slots the platform
// filled so the runner's usage-cap scope keeps metering them as ONE
// shared meter.
func TestPlatformTier_credentiallessRunGetsThePlatformCredentials(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKey(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderOpenAI, "sk-openai-platform")
	oauth := secrets.NewMemoryOAuthStore()
	seedOAuth(t, oauth, sealer, secrets.PlatformOwnerKey, "sk-ant-platform")

	p := &Publisher{
		apiKeys:      keys,
		oauthForfait: oauth,
		runSecrets:   secrets.NewMemoryRunSecretsStore(),
		sealer:       sealer,
		logger:       testLogger(),
	}
	rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)

	b := resolveBundle(t, p, rs, sealer, "run-1", "team1", "webhook:cfg-1")
	if got := b.APIKeys[secrets.ProviderOpenAI]; got != "sk-openai-platform" {
		t.Fatalf("openai key = %q, want the platform key — the tier is not wired", got)
	}
	if got := string(b.OAuthCredentials["claude_code"]); !contains(got, "sk-ant-platform") {
		t.Fatalf("claude_code blob = %q, want the platform forfait", got)
	}
	for _, slot := range []string{"openai", "claude_code"} {
		if !b.PlatformSourced[slot] {
			t.Errorf("PlatformSourced = %v, missing %q — the usage cap would meter this per tenant", b.PlatformSourced, slot)
		}
	}
}

// Two platform keys on ONE wire family (anthropic + zai both map to
// "anthropic-wire") — fillable() lets only one through, and which one must
// be DETERMINISTIC across launches, not a coin-flip on Go's randomised map
// iteration. allKnownProviders order fixes anthropic (index 0) as the
// winner every time.
func TestPlatformTier_sameWireFamilyPicksADeterministicWinner(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	keys := secrets.NewMemoryApiKeyStore()
	seedKey(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderAnthropic, "sk-ant-platform")
	seedKey(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderZAI, "sk-zai-platform")

	// Many independent resolutions: without the fix, map-iteration order
	// flips the winner between them and this loop is flaky.
	for i := 0; i < 30; i++ {
		p := &Publisher{
			apiKeys:    keys,
			runSecrets: secrets.NewMemoryRunSecretsStore(),
			sealer:     sealer,
			logger:     testLogger(),
		}
		rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)
		b := resolveBundle(t, p, rs, sealer, "run-1", "team1", "webhook:cfg-1")
		if b.APIKeys[secrets.ProviderAnthropic] != "sk-ant-platform" || b.APIKeys[secrets.ProviderZAI] != "" {
			t.Fatalf("iter %d: anthropic=%q zai=%q — same-wire winner is non-deterministic",
				i, b.APIKeys[secrets.ProviderAnthropic], b.APIKeys[secrets.ProviderZAI])
		}
	}
}

// Per-slot gap-fill: a tenant credential keeps its slot, the platform only
// funds the families the run still lacks — matching the env fallback's
// per-provider semantics.
func TestPlatformTier_tenantKeyWinsItsSlotPlatformFillsTheRest(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	keys := secrets.NewMemoryApiKeyStore()
	seedKey(t, keys, sealer, "team1", secrets.ProviderAnthropic, "sk-ant-tenant")
	seedKey(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderAnthropic, "sk-ant-platform")
	seedKey(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderOpenAI, "sk-openai-platform")

	p := &Publisher{
		apiKeys:    keys,
		runSecrets: secrets.NewMemoryRunSecretsStore(),
		sealer:     sealer,
		logger:     testLogger(),
	}
	rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)

	b := resolveBundle(t, p, rs, sealer, "run-1", "team1", "alice")
	if got := b.APIKeys[secrets.ProviderAnthropic]; got != "sk-ant-tenant" {
		t.Fatalf("anthropic key = %q, want the tenant's own", got)
	}
	if got := b.APIKeys[secrets.ProviderOpenAI]; got != "sk-openai-platform" {
		t.Fatalf("openai key = %q, want the platform gap-fill", got)
	}
	if b.PlatformSourced["anthropic"] || !b.PlatformSourced["openai"] {
		t.Errorf("PlatformSourced = %v, want exactly the gap-filled slot", b.PlatformSourced)
	}
}

// The wire-family guard: the claude_code delegate ranks a ctx API key above
// a ctx OAuth dir, so filling the platform's anthropic key next to a
// tenant's own forfait would silently make every call spend the platform
// key. The platform must leave the whole anthropic wire alone.
func TestPlatformTier_neverShadowsATenantsForfaitOnTheSameWire(t *testing.T) {
	sealer, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	keys := secrets.NewMemoryApiKeyStore()
	seedKey(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderAnthropic, "sk-ant-platform")
	oauth := secrets.NewMemoryOAuthStore()
	seedOAuth(t, oauth, sealer, "alice", "sk-ant-own-forfait")

	p := &Publisher{
		apiKeys:      keys,
		oauthForfait: oauth,
		runSecrets:   secrets.NewMemoryRunSecretsStore(),
		sealer:       sealer,
		logger:       testLogger(),
	}
	rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)

	b := resolveBundle(t, p, rs, sealer, "run-1", "team1", "alice")
	if got := b.APIKeys[secrets.ProviderAnthropic]; got != "" {
		t.Fatalf("anthropic key = %q injected next to the tenant's own forfait — it would shadow it", got)
	}
	if len(b.PlatformSourced) != 0 {
		t.Errorf("PlatformSourced = %v, want none", b.PlatformSourced)
	}
}

// Order between the two backstops: the pool is asked FIRST (as it was when
// the platform credential lived in env, below the pool), and a granted run
// runs on its donor alone — filling alongside would outrank the lent
// credential while still consuming the donor's quota and slot.
func TestPlatformTier_poolGrantSuppressesThePlatformFill(t *testing.T) {
	f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 5})
	keys := secrets.NewMemoryApiKeyStore()
	// Seed with the FIXTURE's sealer — the publisher can only open what
	// its own master key sealed.
	seedKey(t, keys, f.sealer, secrets.PlatformTenantID, secrets.ProviderAnthropic, "sk-ant-platform")
	f.pub.apiKeys = keys

	bundle, creds := f.resolve(t, "run-1", nil)
	if creds.grant == nil {
		t.Fatal("no grant — the pool should have served this credential-less run")
	}
	if got := bundle.APIKeys[secrets.ProviderAnthropic]; got != "" {
		t.Fatalf("platform key %q filled next to the donor's grant — the donation would never be drawn on", got)
	}
	if len(bundle.PlatformSourced) != 0 {
		t.Errorf("PlatformSourced = %v, want none on a pool-served run", bundle.PlatformSourced)
	}
}

// erroringApiKeyStore fails reads under the platform sentinel only — the
// tenant tier keeps working, isolating the platform tier's degradation.
type erroringApiKeyStore struct{ secrets.ApiKeyStore }

func (e erroringApiKeyStore) ListByTeam(ctx context.Context, teamID, userID string) ([]secrets.ApiKey, error) {
	if teamID == secrets.PlatformTenantID {
		return nil, errors.New("mongo down")
	}
	return e.ApiKeyStore.ListByTeam(ctx, teamID, userID)
}

// A degraded platform store must not fail a launch the env fallback can
// still serve: best-effort, like the pool.
func TestPlatformTier_storeErrorDegradesToNoOp(t *testing.T) {
	slr, _ := secrets.NewAESGCMSealer(make([]byte, 32))
	p := &Publisher{
		apiKeys:    erroringApiKeyStore{secrets.NewMemoryApiKeyStore()},
		runSecrets: secrets.NewMemoryRunSecretsStore(),
		sealer:     slr,
		logger:     iterlog.New(iterlog.LevelError, nil),
	}
	rs := p.runSecrets.(*secrets.MemoryRunSecretsStore)

	b := resolveBundle(t, p, rs, slr, "run-1", "team1", "alice")
	if len(b.APIKeys) != 0 || len(b.PlatformSourced) != 0 {
		t.Fatalf("bundle = %+v, want empty on a degraded platform store", b)
	}
}
