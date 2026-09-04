package credpool

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type harness struct {
	broker  *Broker
	pools   *MemoryPoolStore
	pledges *MemoryPledgeStore
	leases  *MemoryLeaseStore
	ledger  *MemoryLedger
	oauth   *secrets.MemoryOAuthStore
	apiKeys secrets.ApiKeyStore
	sealer  secrets.Sealer
	now     time.Time
}

const testOrg = "org-1"

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		pools:   NewMemoryPoolStore(),
		pledges: NewMemoryPledgeStore(),
		leases:  NewMemoryLeaseStore(),
		ledger:  NewMemoryLedger(),
		oauth:   secrets.NewMemoryOAuthStore(),
		apiKeys: secrets.NewMemoryApiKeyStore(),
		now:     time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC),
	}
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	h.sealer = sealer
	h.broker = NewBroker(BrokerConfig{
		Pools: h.pools, Pledges: h.pledges, Leases: h.leases, Ledger: h.ledger,
		OAuth: h.oauth, APIKeys: h.apiKeys, Sealer: sealer,
		Logger: iterlog.New(iterlog.LevelError, nil),
		Now:    func() time.Time { return h.now },
	})
	if h.broker == nil {
		t.Fatal("NewBroker returned nil with every dependency wired")
	}
	if err := h.pools.Upsert(context.Background(), Pool{ID: "pool-1", OrgID: testOrg, Enabled: true}); err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	return h
}

func (h *harness) donor(t *testing.T, userID string, lim Limits, mutate ...func(*Pledge)) Pledge {
	t.Helper()
	ctx := context.Background()
	// A real sealed credential, shaped like the blob Claude Code writes —
	// the broker must unseal exactly what the OAuth store holds.
	blob, _ := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{"accessToken": "sk-ant-oat-" + userID},
	})
	sealed, err := secrets.SealOAuthPayload(h.sealer, userID, secrets.OAuthKindClaudeCode, blob)
	if err != nil {
		t.Fatalf("seal donor credential: %v", err)
	}
	if err := h.oauth.Upsert(ctx, secrets.OAuthRecord{
		UserID: userID, Kind: secrets.OAuthKindClaudeCode, SealedPayload: sealed,
	}); err != nil {
		t.Fatalf("seed oauth: %v", err)
	}
	p := Pledge{
		ID: PledgeID(userID, SourceOAuth, string(secrets.OAuthKindClaudeCode)), PoolID: "pool-1",
		UserID: userID, Credential: Credential{Source: SourceOAuth, Ref: string(secrets.OAuthKindClaudeCode)},
		Enabled: true, Health: HealthOK, Limits: lim,
	}
	for _, m := range mutate {
		m(&p)
	}
	if err := h.pledges.Upsert(ctx, p); err != nil {
		t.Fatalf("seed pledge: %v", err)
	}
	return p
}

func (h *harness) request(runID string) Request {
	return Request{
		RunID: runID, OrgID: testOrg, TenantID: "team-1", UserID: "requester",
		BotID: "docs-refresh", Wants: []Credential{{Source: SourceOAuth, Ref: string(secrets.OAuthKindClaudeCode)}},
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestBroker_nilIsDisabled(t *testing.T) {
	var b *Broker
	if _, err := b.Acquire(context.Background(), Request{RunID: "r"}); !errors.Is(err, ErrNoDonor) {
		t.Errorf("nil broker Acquire = %v, want ErrNoDonor", err)
	}
	if err := b.Report(context.Background(), "r", Outcome{}); err != nil {
		t.Errorf("nil broker Report = %v, want nil", err)
	}
	if n, err := b.ReleaseExpired(context.Background(), 10); n != 0 || err != nil {
		t.Errorf("nil broker ReleaseExpired = (%d, %v), want (0, nil)", n, err)
	}
}

func TestBroker_acquireServesTheDonorsRealCredential(t *testing.T) {
	h := newHarness(t)
	h.donor(t, "alice", Limits{MaxUSDPerDay: 5})

	grant, err := h.broker.Acquire(context.Background(), h.request("run-1"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if grant.DonorID != "alice" {
		t.Errorf("donor = %q, want alice", grant.DonorID)
	}
	// The payload must be the donor's actual blob, unsealed — not a stub
	// the test handed in. This is what ends up in the run bundle.
	var got map[string]map[string]string
	if err := json.Unmarshal(grant.Payload, &got); err != nil {
		t.Fatalf("granted payload is not the stored credential JSON: %v", err)
	}
	if got["claudeAiOauth"]["accessToken"] != "sk-ant-oat-alice" {
		t.Errorf("granted token = %q, want alice's", got["claudeAiOauth"]["accessToken"])
	}
	if grant.RemainingUSD != 5 {
		t.Errorf("allowance = %v, want the full 5 (nothing spent yet)", grant.RemainingUSD)
	}

	// The lease is the accountability record: who ran what on whose quota.
	lease, err := h.leases.GetOpenByRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("lease not recorded: %v", err)
	}
	if lease.DonorID != "alice" || lease.RequesterID != "requester" || lease.BotID != "docs-refresh" {
		t.Errorf("lease = %+v, want the donor/requester/bot triple", lease)
	}
}

func TestBroker_noPoolOrDisabledPool(t *testing.T) {
	h := newHarness(t)
	h.donor(t, "alice", Limits{})

	// Unknown org.
	req := h.request("run-1")
	req.OrgID = "org-nope"
	if _, err := h.broker.Acquire(context.Background(), req); !errors.Is(err, ErrNoDonor) {
		t.Errorf("unknown org = %v, want ErrNoDonor", err)
	}

	// Operator master switch off.
	_ = h.pools.Upsert(context.Background(), Pool{ID: "pool-1", OrgID: testOrg, Enabled: false})
	if _, err := h.broker.Acquire(context.Background(), h.request("run-2")); !errors.Is(err, ErrNoDonor) {
		t.Errorf("disabled pool = %v, want ErrNoDonor", err)
	}
}

func TestBroker_killSwitchTakesEffectAtNextAcquire(t *testing.T) {
	h := newHarness(t)
	p := h.donor(t, "alice", Limits{})

	if _, err := h.broker.Acquire(context.Background(), h.request("run-1")); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	p.Enabled = false
	if err := h.pledges.Upsert(context.Background(), p); err != nil {
		t.Fatalf("flip kill switch: %v", err)
	}
	if _, err := h.broker.Acquire(context.Background(), h.request("run-2")); !errors.Is(err, ErrNoDonor) {
		t.Errorf("after the kill switch = %v, want ErrNoDonor", err)
	}
}

func TestBroker_fairnessPrefersTheLeastConsumedDonor(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxUSDPerDay: 10})
	h.donor(t, "bob", Limits{MaxUSDPerDay: 10})

	// Alice has already given $8 of her 10 today; Bob nothing.
	if err := h.ledger.AddSpend(ctx, PledgeID("alice", SourceOAuth, "claude_code"), h.now, 8, 0, 0); err != nil {
		t.Fatalf("seed spend: %v", err)
	}
	grant, err := h.broker.Acquire(ctx, h.request("run-1"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if grant.DonorID != "bob" {
		t.Errorf("served by %q, want bob (the least consumed)", grant.DonorID)
	}
}

func TestBroker_fairnessIsProportional(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// A generous donor and a modest one. Ranking on RAW spend would drain
	// the modest pledge first; ranking on the fraction of what each
	// OFFERED is what makes a small contribution safe to make.
	h.donor(t, "generous", Limits{MaxUSDPerDay: 100})
	h.donor(t, "modest", Limits{MaxUSDPerDay: 2})

	// Generous gave $20 (20% of their offer); modest gave $1 (50%).
	_ = h.ledger.AddSpend(ctx, PledgeID("generous", SourceOAuth, "claude_code"), h.now, 20, 0, 0)
	_ = h.ledger.AddSpend(ctx, PledgeID("modest", SourceOAuth, "claude_code"), h.now, 1, 0, 0)

	grant, err := h.broker.Acquire(ctx, h.request("run-1"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if grant.DonorID != "generous" {
		t.Errorf("served by %q, want generous (20%% consumed vs modest's 50%%)", grant.DonorID)
	}
}

func TestBroker_exhaustedDonorYieldsToTheNext(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxUSDPerDay: 5})
	h.donor(t, "bob", Limits{MaxUSDPerDay: 5})
	// Alice is spent; she must be skipped, not fail the acquisition.
	_ = h.ledger.AddSpend(ctx, PledgeID("alice", SourceOAuth, "claude_code"), h.now, 5, 0, 0)

	grant, err := h.broker.Acquire(ctx, h.request("run-1"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if grant.DonorID != "bob" {
		t.Errorf("served by %q, want bob (alice is at her cap)", grant.DonorID)
	}
}

func TestBroker_everyDonorExhausted(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxUSDPerDay: 5})
	_ = h.ledger.AddSpend(ctx, PledgeID("alice", SourceOAuth, "claude_code"), h.now, 5, 0, 0)

	if _, err := h.broker.Acquire(ctx, h.request("run-1")); !errors.Is(err, ErrNoDonor) {
		t.Errorf("Acquire = %v, want ErrNoDonor", err)
	}
	// A refused admission must not leave the donor's run counter inflated —
	// otherwise a run-per-day cap would erode with every rejected launch.
	day, _, err := h.ledger.Usage(ctx, PledgeID("alice", SourceOAuth, "claude_code"), h.now)
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if day.Runs != 0 {
		t.Errorf("day runs = %d, want 0 — the refused reservation was not rolled back", day.Runs)
	}
}

func TestBroker_concurrencyCap(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxConcurrentRuns: 1})

	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if _, err := h.broker.Acquire(ctx, h.request("run-2")); !errors.Is(err, ErrNoDonor) {
		t.Errorf("second concurrent Acquire = %v, want ErrNoDonor", err)
	}
	// Reporting the first run frees the slot.
	if err := h.broker.Report(ctx, "run-1", Outcome{CostUSD: 0.5}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if _, err := h.broker.Acquire(ctx, h.request("run-3")); err != nil {
		t.Errorf("after release = %v, want a grant", err)
	}
}

func TestBroker_runsPerDayCap(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxRunsPerDay: 2})

	for i, run := range []string{"run-1", "run-2"} {
		if _, err := h.broker.Acquire(ctx, h.request(run)); err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
		if err := h.broker.Report(ctx, run, Outcome{}); err != nil {
			t.Fatalf("Report %d: %v", i, err)
		}
	}
	if _, err := h.broker.Acquire(ctx, h.request("run-3")); !errors.Is(err, ErrNoDonor) {
		t.Errorf("third run = %v, want ErrNoDonor (cap is 2/day)", err)
	}
	// A new day reopens the offer with no intervention.
	h.now = h.now.Add(24 * time.Hour)
	if _, err := h.broker.Acquire(ctx, h.request("run-4")); err != nil {
		t.Errorf("next day = %v, want a grant", err)
	}
}

func TestBroker_donorNeverServesTheirOwnRun(t *testing.T) {
	h := newHarness(t)
	h.donor(t, "alice", Limits{})

	req := h.request("run-1")
	req.UserID = "alice"
	if _, err := h.broker.Acquire(context.Background(), req); !errors.Is(err, ErrNoDonor) {
		t.Errorf("Acquire = %v, want ErrNoDonor — a donor lending to themselves is bookkeeping for nothing", err)
	}
}

func TestBroker_reportChargesTheDonorAndClosesTheLease(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxUSDPerDay: 10})

	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := h.broker.Report(ctx, "run-1", Outcome{CostUSD: 1.25, InputTokens: 1000, OutputTokens: 200}); err != nil {
		t.Fatalf("Report: %v", err)
	}

	day, week, err := h.ledger.Usage(ctx, PledgeID("alice", SourceOAuth, "claude_code"), h.now)
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if day.CostUSD != 1.25 || week.CostUSD != 1.25 {
		t.Errorf("day/week cost = %v/%v, want 1.25 in both buckets", day.CostUSD, week.CostUSD)
	}
	if day.InputTokens != 1000 || day.OutputTokens != 200 {
		t.Errorf("tokens = %d/%d, want 1000/200", day.InputTokens, day.OutputTokens)
	}

	// The next grant sees the reduced allowance — this is the number that
	// becomes the next run's cost ceiling.
	grant, err := h.broker.Acquire(ctx, h.request("run-2"))
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	if grant.RemainingUSD != 8.75 {
		t.Errorf("allowance = %v, want 8.75", grant.RemainingUSD)
	}
}

func TestBroker_reportIsIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxUSDPerDay: 10})
	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := h.broker.Report(ctx, "run-1", Outcome{CostUSD: 2}); err != nil {
			t.Fatalf("Report %d: %v", i, err)
		}
	}
	day, _, _ := h.ledger.Usage(ctx, PledgeID("alice", SourceOAuth, "claude_code"), h.now)
	if day.CostUSD != 2 {
		t.Errorf("cost = %v, want 2 — a redelivered report double-charged the donor", day.CostUSD)
	}
}

func TestBroker_reportWithoutLeaseIsNotAnError(t *testing.T) {
	h := newHarness(t)
	// Most runs never touch the pool; reporting them must be a no-op, not
	// an error the runner has to special-case.
	if err := h.broker.Report(context.Background(), "run-unknown", Outcome{CostUSD: 3}); err != nil {
		t.Errorf("Report on an unleased run = %v, want nil", err)
	}
}

func TestBroker_spendIsChargedToTheDayThatAdmittedTheRun(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.now = time.Date(2026, 8, 3, 23, 50, 0, 0, time.UTC)
	h.donor(t, "alice", Limits{MaxUSDPerDay: 10})
	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// The run crosses midnight before reporting.
	acquired := h.now
	h.now = h.now.Add(30 * time.Minute)
	if err := h.broker.Report(ctx, "run-1", Outcome{CostUSD: 4}); err != nil {
		t.Fatalf("Report: %v", err)
	}

	day, _, _ := h.ledger.Usage(ctx, PledgeID("alice", SourceOAuth, "claude_code"), acquired)
	if day.CostUSD != 4 {
		t.Errorf("spend landed on the wrong day: %s shows %v, want 4 — a run admitted under yesterday's allowance must debit yesterday",
			dayKey(acquired), day.CostUSD)
	}
	next, _, _ := h.ledger.Usage(ctx, PledgeID("alice", SourceOAuth, "claude_code"), h.now)
	if next.CostUSD != 0 {
		t.Errorf("%s shows %v, want 0", dayKey(h.now), next.CostUSD)
	}
}

func TestBroker_usageWindowPutsTheDonorToRest(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{})
	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	reset := h.now.Add(3 * time.Hour)
	if err := h.broker.Report(ctx, "run-1", Outcome{
		Condition: ConditionUsageWindow, CooldownUntil: reset,
	}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if _, err := h.broker.Acquire(ctx, h.request("run-2")); !errors.Is(err, ErrNoDonor) {
		t.Errorf("during cooldown = %v, want ErrNoDonor", err)
	}
	// Past the reset the donor returns on their own — no background job.
	h.now = reset.Add(2 * time.Minute)
	if _, err := h.broker.Acquire(ctx, h.request("run-3")); err != nil {
		t.Errorf("after the reset = %v, want a grant", err)
	}
}

func TestBroker_usageWindowWithoutAResetUsesABoundedWait(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{})
	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// The provider said "exhausted" but nothing parsed as a reset instant.
	if err := h.broker.Report(ctx, "run-1", Outcome{Condition: ConditionUsageWindow}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	p, _ := h.pledges.Get(ctx, PledgeID("alice", SourceOAuth, "claude_code"))
	if p.CooldownUntil == nil || !p.CooldownUntil.After(h.now) {
		t.Fatalf("cooldown = %v, want a bounded future instant", p.CooldownUntil)
	}
	if p.CooldownUntil.After(h.now.Add(2 * time.Hour)) {
		t.Errorf("blind cooldown = %v, want something short (≈1h), not a speculative week", p.CooldownUntil)
	}
}

func TestBroker_repeatedAuthFailuresParkTheDonor(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{})

	// Drive the reported condition through the public path for the first
	// failure, then the transition directly — what matters here is the
	// threshold, not which donor the ranking happened to pick.
	id := PledgeID("alice", SourceOAuth, "claude_code")
	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := h.broker.Report(ctx, "run-1", Outcome{Condition: ConditionAuthFailed}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	p, _ := h.pledges.Get(ctx, id)
	if p.Health != HealthOK {
		t.Errorf("one failure parked the donor (%s) — a single blip must not evict someone who did nothing wrong", p.Health)
	}

	if _, err := h.broker.Acquire(ctx, h.request("run-2")); err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	if err := h.broker.Report(ctx, "run-2", Outcome{Condition: ConditionAuthFailed}); err != nil {
		t.Fatalf("second Report: %v", err)
	}
	p, _ = h.pledges.Get(ctx, id)
	if p.Health != HealthAuthFailed {
		t.Errorf("health = %s, want auth_failed after repeated rejections", p.Health)
	}
	if p.HealthDetail == "" {
		t.Error("an unhealthy pledge must tell the donor what to do about it")
	}
	// And it must actually leave the rotation.
	if _, err := h.broker.Acquire(ctx, h.request("run-3")); !errors.Is(err, ErrNoDonor) {
		t.Errorf("a parked donor was still selected: %v", err)
	}
}

func TestBroker_successResetsTheAuthFailureStreak(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{})
	id := PledgeID("alice", SourceOAuth, "claude_code")

	h.broker.noteAuthFailure(ctx, id)
	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := h.broker.Report(ctx, "run-1", Outcome{CostUSD: 0.1}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	p, _ := h.pledges.Get(ctx, id)
	if p.ConsecutiveAuthFailures != 0 {
		t.Errorf("streak = %d, want 0 — unrelated failures weeks apart must not accumulate into an eviction", p.ConsecutiveAuthFailures)
	}
}

func TestBroker_disconnectedCredentialParksThePledge(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{})
	// The donor disconnected the subscription but left the pledge behind.
	if err := h.oauth.Delete(ctx, "alice", secrets.OAuthKindClaudeCode); err != nil {
		t.Fatalf("delete oauth: %v", err)
	}

	if _, err := h.broker.Acquire(ctx, h.request("run-1")); !errors.Is(err, ErrNoDonor) {
		t.Errorf("Acquire = %v, want ErrNoDonor", err)
	}
	p, _ := h.pledges.Get(ctx, PledgeID("alice", SourceOAuth, "claude_code"))
	if p.Health != HealthTokenExpired {
		t.Errorf("health = %s, want token_expired — re-discovering this on every launch is wasted work", p.Health)
	}
	// And the reservation must have been given back.
	day, _, _ := h.ledger.Usage(ctx, PledgeID("alice", SourceOAuth, "claude_code"), h.now)
	if day.Runs != 0 {
		t.Errorf("day runs = %d, want 0 — the reserved unit leaked", day.Runs)
	}
}

func TestBroker_expiredNonRefreshableCredentialIsParked(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{})
	rec, _ := h.oauth.Get(ctx, "alice", secrets.OAuthKindClaudeCode)
	past := h.now.Add(-time.Hour)
	rec.AccessTokenExpiresAt = &past
	rec.NotRefreshable = true
	_ = h.oauth.Upsert(ctx, rec)

	if _, err := h.broker.Acquire(ctx, h.request("run-1")); !errors.Is(err, ErrNoDonor) {
		t.Errorf("Acquire = %v, want ErrNoDonor", err)
	}
	p, _ := h.pledges.Get(ctx, PledgeID("alice", SourceOAuth, "claude_code"))
	if p.Health != HealthTokenExpired {
		t.Errorf("health = %s, want token_expired", p.Health)
	}
}

func TestBroker_releaseExpiredFreesAbandonedLeases(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxConcurrentRuns: 1})
	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// The pod is killed: nothing ever reports. Without the sweeper the
	// donor's only slot would stay consumed.
	h.now = h.now.Add(DefaultLeaseTTL + time.Minute)
	freed, err := h.broker.ReleaseExpired(ctx, 10)
	if err != nil {
		t.Fatalf("ReleaseExpired: %v", err)
	}
	if freed != 1 {
		t.Errorf("freed = %d, want 1", freed)
	}
	if _, err := h.broker.Acquire(ctx, h.request("run-2")); err != nil {
		t.Errorf("after the sweep = %v, want a grant", err)
	}
	// No spend is invented for a run that never reported.
	lease, _ := h.leases.GetOpenByRun(ctx, "run-1")
	if lease.RunID != "" {
		t.Fatalf("run-1 still holds an open lease after the sweep: %+v", lease)
	}
	hist, _ := h.leases.ListByDonor(ctx, "alice", 10)
	var swept *Lease
	for i := range hist {
		if hist[i].RunID == "run-1" {
			swept = &hist[i]
		}
	}
	if swept == nil {
		t.Fatal("run-1's lease vanished from the donor's history")
	}
	if swept.CostUSD != 0 || swept.Outcome != OutcomeAbandoned {
		t.Errorf("abandoned lease = (%v, %q), want (0, \"abandoned\")", swept.CostUSD, swept.Outcome)
	}
}

func TestBroker_resumeReplacesTheLeaseInsteadOfStacking(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxConcurrentRuns: 1})

	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	// A resume re-acquires for the SAME run. One live lease per run, or the
	// donor's concurrency slot would be consumed twice by one run.
	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil {
		t.Fatalf("re-Acquire on resume: %v", err)
	}
	live, _, err := h.leases.LiveCommitment(ctx, PledgeID("alice", SourceOAuth, "claude_code"), "", h.now)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if live != 1 {
		t.Errorf("live leases = %d, want 1", live)
	}
}

func TestBroker_kindIsHonoured(t *testing.T) {
	h := newHarness(t)
	h.donor(t, "alice", Limits{}) // claude_code only

	req := h.request("run-1")
	req.Wants = []Credential{{Source: SourceOAuth, Ref: string(secrets.OAuthKindCodex)}}
	if _, err := h.broker.Acquire(context.Background(), req); !errors.Is(err, ErrNoDonor) {
		t.Errorf("Acquire = %v, want ErrNoDonor — a claude_code pledge cannot serve a codex request", err)
	}
}

// Every audience dial beyond the owning org must be REACHABLE from where
// it admits. Resolving the pool by the requester's own org made all of them
// dead configuration — the setting could be saved and displayed while never
// changing a single launch.
func TestBroker_audienceDialsReachAcrossOrgs(t *testing.T) {
	foreign := func(runID string) Request {
		return Request{
			RunID: runID, OrgID: "org-elsewhere", TenantID: "team-x", UserID: "dave",
			Wants: []Credential{{Source: SourceOAuth, Ref: string(secrets.OAuthKindClaudeCode)}},
		}
	}
	cases := []struct {
		name string
		aud  Audience
	}{
		{"team allow-list", Audience{Teams: []string{"team-x"}}},
		{"org allow-list", Audience{Orgs: []string{"org-elsewhere"}}},
		{"all teams", Audience{AllTeams: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			h.donor(t, "alice", Limits{})
			if _, err := h.broker.Acquire(ctx, foreign("run-0")); !errors.Is(err, ErrNoDonor) {
				t.Fatalf("baseline (default audience) = %v, want ErrNoDonor", err)
			}
			if err := h.pools.Upsert(ctx, Pool{ID: "pool-1", OrgID: testOrg, Enabled: true, Audience: tc.aud}); err != nil {
				t.Fatalf("set audience: %v", err)
			}
			if _, err := h.broker.Acquire(ctx, foreign("run-1")); err != nil {
				t.Errorf("Acquire = %v, want a grant — this audience dial is unreachable", err)
			}
		})
	}
}

// When several pools would serve, the requester's own org pool wins: a team
// must not be diverted onto a community pool that happens to sort earlier.
func TestBroker_ownOrgPoolWinsOverACommunityOne(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// "pool-0" sorts before "pool-1" and admits everyone.
	if err := h.pools.Upsert(ctx, Pool{
		ID: "pool-0", OrgID: "org-community", Enabled: true, Audience: Audience{AllTeams: true},
	}); err != nil {
		t.Fatalf("seed community pool: %v", err)
	}
	h.donor(t, "alice", Limits{}) // in pool-1, the requester's own org pool

	grant, err := h.broker.Acquire(ctx, h.request("run-1"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	lease, _ := h.leases.GetOpenByRun(ctx, "run-1")
	if lease.PoolID != "pool-1" {
		t.Errorf("served by pool %q (donor %s), want the requester's own org pool", lease.PoolID, grant.DonorID)
	}
}

func TestBroker_reciprocityAdmitsAForeignDonor(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{})
	h.donor(t, "carol", Limits{})

	// Carol launches from a team outside the pool's org.
	req := Request{
		RunID: "run-1", OrgID: "org-elsewhere", TenantID: "team-x", UserID: "carol",
		Wants: []Credential{{Source: SourceOAuth, Ref: string(secrets.OAuthKindClaudeCode)}},
	}
	if _, err := h.broker.Acquire(ctx, req); !errors.Is(err, ErrNoDonor) {
		t.Fatalf("with reciprocity off = %v, want ErrNoDonor", err)
	}

	_ = h.pools.Upsert(ctx, Pool{
		ID: "pool-1", OrgID: testOrg, Enabled: true,
		Audience: Audience{Contributors: true},
	})
	grant, err := h.broker.Acquire(ctx, req)
	if err != nil {
		t.Fatalf("with reciprocity on = %v, want a grant", err)
	}
	if grant.DonorID != "alice" {
		t.Errorf("served by %q, want alice (carol cannot serve herself)", grant.DonorID)
	}

	// Reciprocity is earned by an ACTIVE contribution: pausing your own
	// sharing stops your borrowing too.
	carol, _ := h.pledges.Get(ctx, PledgeID("carol", SourceOAuth, "claude_code"))
	carol.Enabled = false
	_ = h.pledges.Upsert(ctx, carol)
	req.RunID = "run-2"
	if _, err := h.broker.Acquire(ctx, req); !errors.Is(err, ErrNoDonor) {
		t.Errorf("paused contributor = %v, want ErrNoDonor", err)
	}
}


// Every abstention arrives as a *NoDonorError naming WHY the pool did
// not serve — the fix for #654, which observed a run fall silently
// through the pool tier onto the platform credential ("half a day of
// investigation on a path that decides who pays"). Each of the four
// abstention sites reports the reason it decided on, and all four
// still unwrap to ErrNoDonor so callers using errors.Is keep working.
func TestBroker_AbstentionCarriesTypedReason(t *testing.T) {
	t.Run("nil broker → pool_disabled", func(t *testing.T) {
		var b *Broker
		_, err := b.Acquire(context.Background(), Request{RunID: "r"})
		if !errors.Is(err, ErrNoDonor) {
			t.Fatalf("errors.Is(err, ErrNoDonor) = false; got %v", err)
		}
		var nd *NoDonorError
		if !errors.As(err, &nd) {
			t.Fatalf("errors.As(*NoDonorError) = false; got %T %v", err, err)
		}
		if nd.Reason != ReasonPoolDisabled {
			t.Errorf("reason = %q, want %q", nd.Reason, ReasonPoolDisabled)
		}
	})

	t.Run("no enabled pool → no_enabled_pool", func(t *testing.T) {
		h := newHarness(t)
		_ = h.pools.Upsert(context.Background(), Pool{ID: "pool-1", OrgID: testOrg, Enabled: false})
		req := h.request("r1")
		req.Wants = []Credential{{Source: SourceOAuth, Ref: string(secrets.OAuthKindClaudeCode)}}
		_, err := h.broker.Acquire(context.Background(), req)
		var nd *NoDonorError
		if !errors.As(err, &nd) {
			t.Fatalf("errors.As failed on %v", err)
		}
		if nd.Reason != ReasonNoEnabledPool {
			t.Errorf("reason = %q, want %q", nd.Reason, ReasonNoEnabledPool)
		}
	})

	t.Run("audience refuses → audience_rejected + pools_considered", func(t *testing.T) {
		h := newHarness(t)
		// A pool that opens itself only to its own org, called from a
		// different org, is the reference case for audience rejection.
		if err := h.pools.Upsert(context.Background(), Pool{
			ID: "pool-closed", OrgID: "someone-else", Enabled: true,
		}); err != nil {
			t.Fatalf("seed second pool: %v", err)
		}
		_ = h.pools.Upsert(context.Background(), Pool{ID: "pool-1", OrgID: testOrg, Enabled: false}) // leave only the closed one enabled
		req := h.request("r2")
		req.OrgID = "requesting-org" // audience.Allows will refuse
		_, err := h.broker.Acquire(context.Background(), req)
		var nd *NoDonorError
		if !errors.As(err, &nd) {
			t.Fatalf("errors.As failed on %v", err)
		}
		if nd.Reason != ReasonAudienceRejected {
			t.Errorf("reason = %q, want %q", nd.Reason, ReasonAudienceRejected)
		}
		if nd.PoolsConsidered != 1 {
			t.Errorf("pools_considered = %d, want 1", nd.PoolsConsidered)
		}
	})

	t.Run("candidates walked but none served → no_eligible_pledge + counts", func(t *testing.T) {
		h := newHarness(t)
		// One donor whose pledge is DISABLED — passes audience, walks the
		// candidates, and every one declines at eligibility. This is the
		// "every donor is currently unavailable" case, mute in prod.
		p := h.donor(t, "alice", Limits{})
		p.Enabled = false
		if err := h.pledges.Upsert(context.Background(), p); err != nil {
			t.Fatalf("disable pledge: %v", err)
		}
		req := h.request("r3")
		_, err := h.broker.Acquire(context.Background(), req)
		var nd *NoDonorError
		if !errors.As(err, &nd) {
			t.Fatalf("errors.As failed on %v", err)
		}
		if nd.Reason != ReasonNoEligiblePledge {
			t.Errorf("reason = %q, want %q", nd.Reason, ReasonNoEligiblePledge)
		}
		if nd.PoolsConsidered != 1 {
			t.Errorf("pools_considered = %d, want 1 (audience opened one pool)", nd.PoolsConsidered)
		}
		if nd.PledgesConsidered != 1 {
			t.Errorf("pledges_considered = %d, want 1 (one pledge on the pool, disabled)", nd.PledgesConsidered)
		}
	})
}
