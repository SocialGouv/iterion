package credpool

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// Lending a metered API key is the case where the pool moves REAL money:
// every token is on the donor's own invoice, not a slice of a plan they
// already pay for. These pin the boundaries that keeps that safe.

// seedKey stores a sealed, user-scoped key for a donor and pledges it.
func (h *harness) donorKey(t *testing.T, userID, provider string, lim Limits, mutate ...func(*Pledge)) (Pledge, string) {
	t.Helper()
	ctx := context.Background()
	keyID := "key-" + userID + "-" + provider
	sealed, err := secrets.SealAPIKey(h.sealer, keyID, []byte("sk-real-"+userID))
	if err != nil {
		t.Fatalf("seal key: %v", err)
	}
	if err := h.apiKeys.Create(ctx, secrets.ApiKey{
		ID: keyID, TenantID: "team-1", ScopeTeamID: "team-1", ScopeUserID: userID,
		Provider: secrets.Provider(provider), Name: "lent", SealedSecret: sealed,
	}); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	p := Pledge{
		ID:         PledgeID(userID, SourceAPIKey, provider),
		PoolID:     "pool-1",
		UserID:     userID,
		Credential: Credential{Source: SourceAPIKey, Ref: provider, KeyID: keyID},
		Enabled:    true, Health: HealthOK, Limits: lim,
	}
	for _, m := range mutate {
		m(&p)
	}
	if err := h.pledges.Upsert(ctx, p); err != nil {
		t.Fatalf("seed pledge: %v", err)
	}
	return p, keyID
}

func (h *harness) wantKey(runID, provider string) Request {
	return Request{
		RunID: runID, OrgID: testOrg, TenantID: "team-1", UserID: "requester",
		Wants: []Credential{{Source: SourceAPIKey, Ref: provider}},
	}
}

func TestAcquire_servesALentAPIKey(t *testing.T) {
	h := newHarness(t)
	h.donorKey(t, "alice", "anthropic", Limits{MaxUSDPerDay: 5})

	grant, err := h.broker.Acquire(context.Background(), h.wantKey("run-1", "anthropic"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if string(grant.Payload) != "sk-real-alice" {
		t.Errorf("payload = %q, want the donor's actual key", grant.Payload)
	}
	if grant.Source != SourceAPIKey || grant.Ref != "anthropic" {
		t.Errorf("credential = %s, want api_key/anthropic", grant.Credential)
	}
	if !grant.Source.Metered() {
		t.Error("a lent API key must report as metered — its figures are real money, not estimates")
	}
}

// A subscription pledge must not answer a request for a metered key, and
// vice versa: they are billed to different places.
func TestAcquire_doesNotSubstituteOneSourceForTheOther(t *testing.T) {
	h := newHarness(t)
	h.donor(t, "alice", Limits{}) // an OAuth subscription

	if _, err := h.broker.Acquire(context.Background(), h.wantKey("run-1", "anthropic")); !errors.Is(err, ErrNoDonor) {
		t.Errorf("a subscription served an api_key request: %v", err)
	}

	h2 := newHarness(t)
	h2.donorKey(t, "bob", "anthropic", Limits{MaxUSDPerDay: 5})
	if _, err := h2.broker.Acquire(context.Background(), h2.request("run-1")); !errors.Is(err, ErrNoDonor) {
		t.Errorf("an api key served a subscription request: %v", err)
	}
}

// The load-bearing boundary: a pledge must never expose a key that is not
// the donor's own. A team-wide key is the team's to spend, and lending it
// would let one member hand the whole team's credential to the pool.
func TestAcquire_refusesAKeyThatIsNotTheDonorsOwn(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p, keyID := h.donorKey(t, "alice", "anthropic", Limits{MaxUSDPerDay: 5})

	// Re-scope the key to the whole team behind the pledge's back.
	k, err := h.apiKeys.Get(ctx, keyID)
	if err != nil {
		t.Fatalf("get key: %v", err)
	}
	k.ScopeUserID = ""
	if err := h.apiKeys.Update(ctx, k); err != nil {
		t.Fatalf("update key: %v", err)
	}

	if _, err := h.broker.Acquire(ctx, h.wantKey("run-1", "anthropic")); !errors.Is(err, ErrNoDonor) {
		t.Errorf("Acquire = %v, want ErrNoDonor — a team key was lent by one member", err)
	}
	parked, _ := h.pledges.Get(ctx, p.ID)
	if parked.Health == HealthOK {
		t.Error("the pledge stayed healthy — every later launch would rediscover this")
	}
}

// A key whose provider changed under the pledge must not be served as the
// pledged one: it would land in the wrong slot of the run bundle.
func TestAcquire_refusesAKeyWhoseProviderNoLongerMatches(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, keyID := h.donorKey(t, "alice", "anthropic", Limits{MaxUSDPerDay: 5})
	k, _ := h.apiKeys.Get(ctx, keyID)
	k.Provider = secrets.ProviderOpenAI
	if err := h.apiKeys.Update(ctx, k); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := h.broker.Acquire(ctx, h.wantKey("run-1", "anthropic")); !errors.Is(err, ErrNoDonor) {
		t.Errorf("Acquire = %v, want ErrNoDonor", err)
	}
}

func TestAcquire_refusesAnExpiredKey(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, keyID := h.donorKey(t, "alice", "anthropic", Limits{MaxUSDPerDay: 5})
	k, _ := h.apiKeys.Get(ctx, keyID)
	past := h.now.Add(-time.Hour)
	k.ExpiresAt = &past
	if err := h.apiKeys.Update(ctx, k); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := h.broker.Acquire(ctx, h.wantKey("run-1", "anthropic")); !errors.Is(err, ErrNoDonor) {
		t.Errorf("Acquire = %v, want ErrNoDonor", err)
	}
}

// A deleted key parks the pledge rather than failing every launch.
func TestAcquire_deletedKeyParksThePledge(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p, keyID := h.donorKey(t, "alice", "anthropic", Limits{MaxUSDPerDay: 5})
	if err := h.apiKeys.Delete(ctx, keyID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := h.broker.Acquire(ctx, h.wantKey("run-1", "anthropic")); !errors.Is(err, ErrNoDonor) {
		t.Errorf("Acquire = %v, want ErrNoDonor", err)
	}
	parked, _ := h.pledges.Get(ctx, p.ID)
	if parked.Health != HealthTokenExpired {
		t.Errorf("health = %s, want the pledge parked", parked.Health)
	}
	// And the reservation was returned.
	day, _, _ := h.ledger.Usage(ctx, p.ID, h.now)
	if day.Runs != 0 {
		t.Errorf("day runs = %d, want 0 — a refused acquisition kept a unit", day.Runs)
	}
}

// The whole point of the ceilings, on the source where they are real money.
func TestAcquire_meteredKeyIsBoundedByItsDonorsCeiling(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	p, _ := h.donorKey(t, "alice", "anthropic", Limits{MaxUSDPerDay: 4})

	grant, err := h.broker.Acquire(ctx, h.wantKey("run-1", "anthropic"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if grant.RemainingUSD != 4 {
		t.Fatalf("allowance = %v, want 4", grant.RemainingUSD)
	}
	if err := h.broker.Report(ctx, "run-1", Outcome{CostUSD: 4}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if _, err := h.broker.Acquire(ctx, h.wantKey("run-2", "anthropic")); !errors.Is(err, ErrNoDonor) {
		t.Errorf("Acquire = %v, want ErrNoDonor once the donor's real money is spent", err)
	}
	day, _, _ := h.ledger.Usage(ctx, p.ID, h.now)
	if day.CostUSD != 4 {
		t.Errorf("charged %v, want 4", day.CostUSD)
	}
}

// A donor with no api-key store wired must not crash the launch.
func TestAcquire_apiKeyPledgeWithoutAStoreDegrades(t *testing.T) {
	h := newHarness(t)
	h.broker.apiKeys = nil
	h.donorKey(t, "alice", "anthropic", Limits{MaxUSDPerDay: 5})
	if _, err := h.broker.Acquire(context.Background(), h.wantKey("run-1", "anthropic")); !errors.Is(err, ErrNoDonor) {
		t.Errorf("Acquire = %v, want ErrNoDonor", err)
	}
}

func TestPledgeValidate_meteredKeyMustNameTheKey(t *testing.T) {
	p := Pledge{UserID: "alice", Credential: Credential{Source: SourceAPIKey, Ref: "anthropic"}}
	if err := p.Validate(); err == nil {
		t.Error("an api_key pledge without a key id was accepted")
	}
	p.KeyID = "key-1"
	if err := p.Validate(); err != nil {
		t.Errorf("valid pledge rejected: %v", err)
	}
}

func TestCredentialSource_metered(t *testing.T) {
	if SourceOAuth.Metered() {
		t.Error("a subscription must not read as metered — its figures are estimates")
	}
	if !SourceAPIKey.Metered() {
		t.Error("an API key is billed per token; callers rely on this to stop hedging the figures")
	}
	if (CredentialSource("nonsense")).Valid() {
		t.Error("an unknown source was accepted")
	}
}
