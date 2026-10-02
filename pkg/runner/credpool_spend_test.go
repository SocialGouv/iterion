package runner

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// poolHarness wires a real broker with one donor who is already serving a
// run, so the runner's report closes a genuine lease.
type poolHarness struct {
	runner  *Runner
	broker  *credpool.Broker
	pledges *credpool.MemoryPledgeStore
	ledger  *credpool.MemoryLedger
	leases  *credpool.MemoryLeaseStore
}

func newPoolHarness(t *testing.T, limits credpool.Limits) *poolHarness {
	t.Helper()
	return newPoolHarnessForRun(t, limits, "run-1")
}

func newPoolHarnessForRun(t *testing.T, limits credpool.Limits, runID string) *poolHarness {
	t.Helper()
	ctx := context.Background()
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	oauth := secrets.NewMemoryOAuthStore()
	blob := []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-donated"}}`)
	sealed, err := secrets.SealOAuthPayload(sealer, "donor", secrets.OAuthKindClaudeCode, blob)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if err := oauth.Upsert(ctx, secrets.OAuthRecord{
		UserID: "donor", Kind: secrets.OAuthKindClaudeCode, SealedPayload: sealed,
	}); err != nil {
		t.Fatalf("seed oauth: %v", err)
	}

	pools := credpool.NewMemoryPoolStore()
	if err := pools.Upsert(ctx, credpool.Pool{ID: "pool-1", OrgID: "org-1", Enabled: true}); err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	pledges := credpool.NewMemoryPledgeStore()
	if err := pledges.Upsert(ctx, credpool.Pledge{
		ID: credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code"), PoolID: "pool-1",
		UserID: "donor", Credential: credpool.Credential{Source: credpool.SourceOAuth, Ref: "claude_code"},
		Enabled: true, Health: credpool.HealthOK, Limits: limits,
	}); err != nil {
		t.Fatalf("seed pledge: %v", err)
	}
	leases := credpool.NewMemoryLeaseStore()
	ledger := credpool.NewMemoryLedger()
	broker := credpool.NewBroker(credpool.BrokerConfig{
		Pools: pools, Pledges: pledges, Leases: leases, Ledger: ledger,
		OAuth: oauth, Sealer: sealer, Logger: iterlog.New(iterlog.LevelError, nil),
	})
	if broker == nil {
		t.Fatal("broker is nil with every dependency wired")
	}
	// The run is already served — exactly the state the runner reports on.
	if _, err := broker.Acquire(ctx, credpool.Request{
		RunID: runID, OrgID: "org-1", TenantID: "team-1", UserID: "requester",
		Wants: []credpool.Credential{{Source: credpool.SourceOAuth, Ref: string(secrets.OAuthKindClaudeCode)}},
	}); err != nil {
		t.Fatalf("seed lease: %v", err)
	}
	return &poolHarness{
		runner: &Runner{cfg: Config{
			CredPool: broker,
			Logger:   iterlog.New(iterlog.LevelError, nil),
		}},
		broker: broker, pledges: pledges, ledger: ledger, leases: leases,
	}
}

// usageWith drives the PRODUCTION hook so the reported cost is the one the
// real chain produces, not a number the test typed in.
func usageWith(costUSD float64, tokens int) *metricsEmitter {
	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New())
	hooks := model.NewStoreEventHooks(
		context.Background(), usage, "run-1", iterlog.New(iterlog.LevelError, nil), nil,
	)
	hooks.OnDelegateFinished("n1", model.DelegateInfo{
		BackendName: "claude_code", Tokens: tokens, CostUSD: costUSD,
	})
	return usage
}

// The closing half of the loop: what a delegate run actually spent must
// land on the lending contributor's ledger, and free their slot.
func TestRecordPoolSpend_chargesTheDonorAndClosesTheLease(t *testing.T) {
	h := newPoolHarness(t, credpool.Limits{MaxUSDPerDay: 10, MaxConcurrentRuns: 1})
	ctx := context.Background()

	h.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-1"}, usageWith(1.5, 900), nil, false)

	day, _, err := h.ledger.Usage(ctx, credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code"), time.Now().UTC())
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if day.CostUSD != 1.5 {
		t.Errorf("donor charged %v, want 1.5 — the delegate run's spend never reached the pool", day.CostUSD)
	}
	hist, err := h.leases.ListByDonor(ctx, "donor", 10)
	if err != nil || len(hist) != 1 {
		t.Fatalf("donor history = (%d, %v), want 1 lease", len(hist), err)
	}
	lease := hist[0]
	if !lease.Closed {
		t.Error("lease still open — the donor's concurrency slot stays consumed")
	}
}

// A run that never touched the pool must report harmlessly: that is the
// vast majority of them.
func TestRecordPoolSpend_unleasedRunIsANoOp(t *testing.T) {
	h := newPoolHarness(t, credpool.Limits{})
	h.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-elsewhere"}, usageWith(2, 100), nil, false)
	day, _, _ := h.ledger.Usage(context.Background(), credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code"), time.Now().UTC())
	if day.CostUSD != 0 {
		t.Errorf("charged %v for a run that holds no lease", day.CostUSD)
	}
}

// No pool wired at all — the common deployment. Must not panic or block.
func TestRecordPoolSpend_noPoolIsANoOp(t *testing.T) {
	r := &Runner{cfg: Config{Logger: iterlog.New(iterlog.LevelError, nil)}}
	r.recordPoolSpend(&queue.RunMessage{RunID: "run-1"}, usageWith(1, 10), nil, false)
}

// The condition mapping decides whether a donor keeps their place. Getting
// it wrong either evicts someone whose credential is fine, or keeps
// hammering a subscription whose quota window is shut.
func TestClassifyPoolCondition(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		want        credpool.Condition
		wantHasTime bool
	}{
		{"success", nil, credpool.ConditionOK, false},
		{
			// A workflow that failed on its own logic says nothing about
			// the donor's credential.
			"an ordinary workflow failure does not blame the donor",
			errors.New("node judge failed: schema validation"), credpool.ConditionOK, false,
		},
		{
			"provider quota window, with a reset instant",
			&delegate.ErrRateLimited{Kind: delegate.RateLimitKindUsageWindow, ResetAt: time.Now().Add(3 * time.Hour)},
			credpool.ConditionUsageWindow, true,
		},
		{
			"a plain throttle is not a quota window",
			&delegate.ErrRateLimited{Kind: delegate.RateLimitKindTransient},
			credpool.ConditionOK, false,
		},
		{
			// The recovery dispatcher may have re-typed it by the time it
			// reaches the runner.
			"quota window re-typed as a runtime code",
			&runtime.RuntimeError{Code: runtime.ErrCodeUsageLimitBlocked},
			credpool.ConditionUsageWindow, false,
		},
		{
			"rejected credential",
			&delegate.ErrAuthFailed{Provider: "claude_code", Detail: "401"},
			credpool.ConditionAuthFailed, false,
		},
		{
			// Errors are wrapped many layers deep by the time they surface.
			"wrapped rejected credential",
			fmt.Errorf("run failed: %w", &delegate.ErrAuthFailed{Provider: "claude_code"}),
			credpool.ConditionAuthFailed, false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, at := classifyPoolCondition(tc.err, time.Now().UTC())
			if got != tc.want {
				t.Errorf("condition = %q, want %q", got, tc.want)
			}
			if at.IsZero() == tc.wantHasTime {
				t.Errorf("reset instant present = %v, want %v", !at.IsZero(), tc.wantHasTime)
			}
		})
	}
}

// End to end through the runner: a usage-window failure must put the donor
// to rest, so the next run does not walk into the same closed window.
func TestRecordPoolSpend_usageWindowRestsTheDonor(t *testing.T) {
	h := newPoolHarness(t, credpool.Limits{})
	ctx := context.Background()
	reset := time.Now().UTC().Add(2 * time.Hour)

	h.runner.recordPoolSpend(
		&queue.RunMessage{RunID: "run-1", TenantID: "team-1"},
		usageWith(0.2, 50),
		&delegate.ErrRateLimited{Kind: delegate.RateLimitKindUsageWindow, ResetAt: reset},
		false)

	p, err := h.pledges.Get(ctx, credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code"))
	if err != nil {
		t.Fatalf("pledge: %v", err)
	}
	if p.CooldownUntil == nil || !p.CooldownUntil.After(time.Now().UTC()) {
		t.Fatalf("cooldown = %v, want a future instant", p.CooldownUntil)
	}
	if _, err := h.broker.Acquire(ctx, credpool.Request{
		RunID: "run-2", OrgID: "org-1", TenantID: "team-1", UserID: "requester",
		Wants: []credpool.Credential{{Source: credpool.SourceOAuth, Ref: string(secrets.OAuthKindClaudeCode)}},
	}); !errors.Is(err, credpool.ErrNoDonor) {
		t.Errorf("next Acquire = %v, want ErrNoDonor while the donor rests", err)
	}
}

// An auth rejection that the RECOVERY machinery converts into a human
// pause must still park the donor.
//
// Measured on prod: a lent credential whose token had expired stayed
// `active` and first in the pool's rotation after two runs, because by the
// time the runner reports, execErr says only "paused" — the rejection is
// nowhere in it.
//
// Driven by a REAL engine writing through the store the runner hands it.
// The signal is an ENGINE event (node_recovery), and the first version of
// this fix wired the tap onto the executor's emitter only, leaving the
// engine writing to the raw store — the capability was dead in production
// while a test that called observe() by hand stayed green.
func TestRecordPoolSpend_authFailureAbsorbedByRecoveryStillParksTheDonor(t *testing.T) {
	h := newPoolHarness(t, credpool.Limits{MaxUSDPerDay: 10})
	ctx := context.Background()

	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New())
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	wf := &ir.Workflow{
		Name:  "auth_probe",
		Entry: "agent",
		Nodes: map[string]ir.Node{
			"agent": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "agent"}},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{{From: "agent", To: "done"}},
	}
	// The engine gets the SAME wrapper the runner builds. If that wiring
	// regresses, this test goes red.
	// Wired exactly as the runner wires it: the raw store plus the engine's
	// event-observer seam. A store decorator would have hidden the store's
	// optional capabilities from the engine's own probes.
	eng := runtime.New(wf, st, authRejectingExecutor{},
		runtime.WithEventObserver(usage.observe),
		runtime.WithRecoveryDispatch(recovery.Dispatch(recovery.DefaultRecipes())),
		// The skill mirror lands here, not in the package directory (#1803).
		runtime.WithWorkDir(t.TempDir()))
	_ = eng.Run(ctx, "run-1", map[string]any{})

	if !usage.SawAuthFailure() {
		t.Fatal("the engine's own auth classification never reached the runner's observer")
	}

	// The attempt ends PAUSED, not on the auth error — the prod shape.
	h.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-1"}, usage, runtime.ErrRunPaused, false)

	p, err := h.pledges.Get(ctx, credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code"))
	if err != nil {
		t.Fatalf("pledge: %v", err)
	}
	if p.ConsecutiveAuthFailures == 0 {
		t.Fatal("the rejection never reached the donor's health counter — a dead lent credential stays first in the rotation")
	}
}

// authRejectingExecutor fails every node the way a provider rejecting a
// credential does.
type authRejectingExecutor struct{}

func (authRejectingExecutor) Execute(context.Context, ir.Node, map[string]any) (map[string]any, error) {
	return nil, &delegate.ErrAuthFailed{Provider: "claw", Detail: "401 token expired"}
}

// The mirror: an ordinary workflow failure says nothing about the
// credential and must not cost the donor their place.
func TestRecordPoolSpend_ordinaryFailureLeavesTheDonorAlone(t *testing.T) {
	h := newPoolHarness(t, credpool.Limits{MaxUSDPerDay: 10})
	h.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-1"},
		usageWith(1, 10), errors.New("the bot's own logic failed"), false)

	p, err := h.pledges.Get(context.Background(), credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code"))
	if err != nil {
		t.Fatalf("pledge: %v", err)
	}
	if p.ConsecutiveAuthFailures != 0 {
		t.Errorf("auth failures = %d — a bot failing on its own logic blamed the donor's credential", p.ConsecutiveAuthFailures)
	}
}

// TestRecordPoolSpend_aSupersededDeliveryLeavesTheNewerAttemptsLease: the
// run's open lease belongs to the attempt the run was queued for since —
// a superseded delivery ran nothing, and reporting would close that lease
// under the newer attempt, and charge the donor nothing it spent.
func TestRecordPoolSpend_aSupersededDeliveryLeavesTheNewerAttemptsLease(t *testing.T) {
	h := newPoolHarness(t, credpool.Limits{MaxUSDPerDay: 10, MaxConcurrentRuns: 1})
	ctx := context.Background()
	superseded := fmt.Errorf("%w: run run-1 was queued at T2, this delivery was published at T1", runtime.ErrResumeSuperseded)

	h.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-1"}, usageWith(1.5, 900), superseded, false)

	hist, err := h.leases.ListByDonor(ctx, "donor", 10)
	if err != nil || len(hist) != 1 {
		t.Fatalf("donor history = (%d, %v), want 1 lease", len(hist), err)
	}
	if hist[0].Closed {
		t.Fatal("a superseded delivery closed the run's lease, which is the newer attempt's")
	}
	day, _, err := h.ledger.Usage(ctx, credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code"), time.Now().UTC())
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if day.CostUSD != 0 {
		t.Fatalf("donor charged %v by a delivery that ran nothing", day.CostUSD)
	}
}

// TestRecordPoolSpend_aLateReportStaysOnItsAttemptInsideASupersedeWindow:
// an attempt's teardown can take minutes, and an operator resume acquires
// inside that window. The report the runner makes carries the delivery's
// publication, so it lands on the attempt's own (superseded) lease —
// charged once through the stamp — and the successor's lease stays open
// for the successor, whose own spend reaches the donor's ledger too.
func TestRecordPoolSpend_aLateReportStaysOnItsAttemptInsideASupersedeWindow(t *testing.T) {
	h := newPoolHarness(t, credpool.Limits{MaxUSDPerDay: 10, MaxConcurrentRuns: 1})
	ctx := context.Background()

	// The run's first delivery, published after its lease was acquired —
	// stamped by store.PublishAt, never inside that millisecond.
	open, err := h.leases.GetOpenByRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("attempt A's lease: %v", err)
	}
	T_A := store.PublishAt(time.Now().UTC(), open.AcquiredAt)
	msgA := &queue.RunMessage{RunID: "run-1", TenantID: "team-1", PublishedAtRFC: T_A.Format(time.RFC3339Nano)}

	// The resume's acquisition supersedes A's lease and opens its own.
	if _, err := h.broker.Acquire(ctx, credpool.Request{
		RunID: "run-1", OrgID: "org-1", TenantID: "team-1", UserID: "requester",
		Wants: []credpool.Credential{{Source: credpool.SourceOAuth, Ref: "claude_code"}},
	}); err != nil {
		t.Fatalf("resume acquire: %v", err)
	}
	successor, err := h.leases.GetOpenByRun(ctx, "run-1")
	if err != nil || successor.ID == open.ID {
		t.Fatalf("the resume's lease = (%+v, %v), want a new one", successor, err)
	}

	// A's teardown reports, through the production hook.
	h.runner.recordPoolSpend(msgA, usageWith(3, 400), nil, false)
	stamped, err := h.leases.Get(ctx, open.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stamped.Closed || stamped.Outcome != credpool.OutcomeSupersededReported || stamped.CostUSD != 3 {
		t.Fatalf("A's lease after its report: closed=%v outcome=%q cost=%.2f acquiredAt=%v closedAt=%v pub=%v, want closed %q with $3", stamped.Closed, stamped.Outcome, stamped.CostUSD, stamped.AcquiredAt, stamped.ClosedAt, T_A, credpool.OutcomeSupersededReported)
	}
	if now, err := h.leases.GetOpenByRun(ctx, "run-1"); err != nil || now.ID != successor.ID {
		t.Fatalf("the successor lease is not open for the successor: (%+v, %v)", now, err)
	}

	// The successor's own attempt reports through its lease.
	msgB := &queue.RunMessage{RunID: "run-1", TenantID: "team-1", PublishedAtRFC: time.Now().UTC().Add(2 * time.Millisecond).Format(time.RFC3339Nano)}
	h.runner.recordPoolSpend(msgB, usageWith(2, 100), nil, false)
	day, _, err := h.ledger.Usage(ctx, credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code"), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if day.CostUSD != 5 {
		t.Fatalf("the donor's ledger records $%.2f, want 3+2", day.CostUSD)
	}
}

// TestRecordPoolSpend_aGrantlessDeliveryNeverReachesTheBroker: a delivery
// published without a pool grant runs on env credentials — its spend
// belongs to no donor, and its report must never reach the broker, where
// it would be charged to the lease of the attempt the grantless
// publication took the run from.
func TestRecordPoolSpend_aGrantlessDeliveryNeverReachesTheBroker(t *testing.T) {
	h := newPoolHarness(t, credpool.Limits{MaxUSDPerDay: 10, MaxConcurrentRuns: 1})
	ctx := context.Background()
	T_A := store.PublishAt(time.Now().UTC(), time.Now().UTC())
	msg := &queue.RunMessage{RunID: "run-1", TenantID: "team-1", PoolGrantless: true, PublishedAtRFC: T_A.Format(time.RFC3339Nano)}
	h.runner.recordPoolSpend(msg, usageWith(4, 400), nil, false)
	day, _, err := h.ledger.Usage(ctx, credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code"), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if day.CostUSD != 0 {
		t.Fatalf("the donor was charged $%.2f for a grantless delivery", day.CostUSD)
	}
	if open, err := h.leases.GetOpenByRun(ctx, "run-1"); err != nil || open.Closed || open.Outcome != "" {
		t.Fatalf("the grantless report touched the run's lease: (%+v, %v)", open, err)
	}
}
