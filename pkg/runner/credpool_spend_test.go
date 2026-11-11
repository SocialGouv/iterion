package runner

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/credroute"
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
	// donorFingerprint is the record's audit identity (OAuthRecord.Fingerprint,
	// stamped by every real connect): the acquired lease carries it, which is
	// what the fine booking slices by (cred_plan_fine_booking_test.go).
	if err := oauth.Upsert(ctx, secrets.OAuthRecord{
		UserID: "donor", Kind: secrets.OAuthKindClaudeCode, SealedPayload: sealed,
		Fingerprint: donorFingerprint,
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
	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New(), nil, nil)
	hooks := model.NewStoreEventHooks(
		context.Background(), usage, "run-1", iterlog.New(iterlog.LevelError, nil), nil, nil,
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

	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New(), nil, nil)
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

// TestRecordPoolSpend_theMessagesAttemptIDPinsItsLease: a publication
// floored past its marker can follow the resume's acquisition, so the
// publication instant alone would pin the successor's lease. The runner
// reports with the attempt id the message carries, which pins the
// attempt's own lease whatever the instants.
func TestRecordPoolSpend_theMessagesAttemptIDPinsItsLease(t *testing.T) {
	// Two slots: the harness's own run-1 holds one.
	h := newPoolHarness(t, credpool.Limits{MaxUSDPerDay: 10, MaxConcurrentRuns: 2})
	ctx := context.Background()
	want := []credpool.Credential{{Source: credpool.SourceOAuth, Ref: "claude_code"}}
	gA, err := h.broker.Acquire(ctx, credpool.Request{RunID: "run-2", OrgID: "org-1", TenantID: "team-1", UserID: "requester", Wants: want, AttemptID: "A1"})
	if err != nil {
		t.Fatalf("attempt A acquire: %v", err)
	}
	gB, err := h.broker.Acquire(ctx, credpool.Request{RunID: "run-2", OrgID: "org-1", TenantID: "team-1", UserID: "requester", Wants: want, AttemptID: "A2"})
	if err != nil {
		t.Fatalf("resume acquire: %v", err)
	}
	// A's publication follows the resume's acquisition.
	T_A := store.PublishAt(gB.AcquiredAt, gB.AcquiredAt)
	msgA := &queue.RunMessage{RunID: "run-2", TenantID: "team-1", PoolAttemptID: gA.AttemptID, PublishedAtRFC: T_A.Format(time.RFC3339Nano)}

	h.runner.recordPoolSpend(msgA, usageWith(3, 400), nil, false)

	all, err := h.leases.ListByRun(ctx, "run-2")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range all {
		switch l.AttemptID {
		case "A1":
			if l.Outcome != credpool.OutcomeSupersededReported || l.CostUSD != 3 {
				t.Errorf("A's lease after its report: closed=%v outcome=%q cost=%.2f, want %q with $3", l.Closed, l.Outcome, l.CostUSD, credpool.OutcomeSupersededReported)
			}
		case "A2":
			if l.Closed || l.CostUSD != 0 {
				t.Errorf("the resume's lease after A's report: closed=%v outcome=%q cost=%.2f, want open and uncharged", l.Closed, l.Outcome, l.CostUSD)
			}
		}
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

// A chain whose one route met a usage window and another a dead credential
// stops on the window (#3032): the node's code says USAGE_LIMIT_BLOCKED, and
// the rejection lives in the route verdicts the node_recovery event carries.
// When those verdicts cannot be placed on a credential — no route names its
// keys and no credential plan rides the publication — pool health still sees
// the dead credential, read through the same engine-and-recovery wiring the
// runner uses: the coarse answer counts the donor an auth failure rather than
// resting a possibly dead credential on the other route's window and handing
// it back, still dead, when it reopens.
func TestRecordPoolSpend_aDeadDonorBehindAUsageWindowIsStillAnAuthFailure(t *testing.T) {
	h := newPoolHarness(t, credpool.Limits{MaxUSDPerDay: 10})
	ctx := context.Background()

	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New(), nil, nil)
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	wf := &ir.Workflow{
		Name:  "window_and_dead_donor",
		Entry: "agent",
		Nodes: map[string]ir.Node{
			"agent": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "agent"}},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{{From: "agent", To: "done"}},
	}
	eng := runtime.New(wf, st, windowBehindDeadDonorExecutor{},
		runtime.WithEventObserver(usage.observe),
		runtime.WithRecoveryDispatch(recovery.Dispatch(recovery.DefaultRecipes())),
		runtime.WithWorkDir(t.TempDir()))
	execErr := eng.Run(ctx, "run-1", map[string]any{})
	var rt *runtime.RuntimeError
	if !errors.As(execErr, &rt) || rt.Code != runtime.ErrCodeUsageLimitBlocked {
		t.Fatalf("run ended on %v, want the chain's USAGE_LIMIT_BLOCKED — the premise of this witness", execErr)
	}
	if !usage.SawAuthFailure() {
		t.Fatal("the dead donor named in the chain's route verdicts never reached the runner's observer")
	}

	h.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-1"}, usage, execErr, false)

	p, err := h.pledges.Get(ctx, credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code"))
	if err != nil {
		t.Fatalf("pledge: %v", err)
	}
	if p.ConsecutiveAuthFailures == 0 {
		t.Fatalf("auth failures = 0 (cooldown until %v) — the donor's dead credential was rested on the other route's window and comes back into rotation still dead", p.CooldownUntil)
	}
}

// windowBehindDeadDonorExecutor fails every node on an exhausted chain: the
// lent donor's credential rejected, then the next route's five-hour window.
type windowBehindDeadDonorExecutor struct{}

func (windowBehindDeadDonorExecutor) Execute(context.Context, ir.Node, map[string]any) (map[string]any, error) {
	return nil, fmt.Errorf("model: node %q: %w", "agent", &model.ErrChainExhausted{Chain: "claude_code,zai", Errs: []error{
		&delegate.ErrAuthFailed{Provider: delegate.BackendClaudeCode, Detail: "OAuth session expired and could not be refreshed"},
		&delegate.ErrRateLimited{Provider: "zai", Kind: delegate.RateLimitKindUsageWindow,
			Detail: "[1308][Usage limit reached for 5 hour.]", ResetAt: time.Now().Add(5 * time.Hour)},
	}})
}

// The donor's condition is the verdict of the route ITS credential served,
// read from the route verdicts — each names the route it is about, and the
// runner places it on the credential that route spent, the way it books the
// route's spend: the route's own credential keys, or the credential plan's
// row. In both orders of each chain:
//
//   - [the donor's lent subscription → a five-hour window, the team's z.ai
//     key → dead]: the donor rests on its own window and is no auth failure
//     — blamed for the team key, a healthy donor is evicted after two runs and
//     never rested on the window it actually met;
//   - [the donor's subscription → dead, the team key → a window]: the donor is
//     the dead credential, whatever window another route met.
func TestRecordPoolSpend_theDonorTakesTheVerdictOfItsOwnRoute(t *testing.T) {
	reset := time.Now().UTC().Add(5 * time.Hour).Truncate(time.Second)
	// The routes as the executor's walk names them
	// (TestErrChainExhaustedNamesTheRouteOfEveryCause): a claude_code session
	// reports no credential keys and, unhinted, no provider — its session
	// label names the subscription; the claw route reports its keys.
	donor := model.ChainRoute{Backend: delegate.BackendClaudeCode, Model: "claude-sonnet-4-5", Source: "anthropic-oauth"}
	team := model.ChainRoute{Backend: delegate.BackendClaw, Model: "zai/glm-5.3", Provider: "zai",
		Credential: credroute.Spend{Channel: credroute.ChannelRunKey, Slot: "zai", Fingerprint: "fp-team-zai"}}
	window := &delegate.ErrRateLimited{Provider: "anthropic", Kind: delegate.RateLimitKindUsageWindow,
		Detail: "Claude usage limit reached", ResetAt: reset}
	dead := &delegate.ErrAuthFailed{Provider: "zai", Detail: "401 invalid api key"}
	for _, tc := range []struct {
		name     string
		routes   []model.ChainRoute
		errs     []error
		wantDead bool
	}{
		{"donor window then team dead", []model.ChainRoute{donor, team}, []error{window, dead}, false},
		{"team dead then donor window", []model.ChainRoute{team, donor}, []error{dead, window}, false},
		{"donor dead then team window", []model.ChainRoute{donor, team}, []error{dead, window}, true},
		{"team window then donor dead", []model.ChainRoute{team, donor}, []error{window, dead}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPoolHarness(t, credpool.Limits{MaxUSDPerDay: 10})
			ctx := context.Background()
			plan := map[string]string{"agent|anthropic": donorFingerprint}
			usage := newMetricsEmitter(&recordingEmitter{}, metrics.New(), plan, nil)
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatalf("store: %v", err)
			}
			wf := &ir.Workflow{
				Name:  "donor_route_verdict",
				Entry: "agent",
				Nodes: map[string]ir.Node{
					"agent": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "agent"}},
					"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
				},
				Edges: []*ir.Edge{{From: "agent", To: "done"}},
			}
			chain := &model.ErrChainExhausted{Chain: "routes", Errs: tc.errs, Routes: tc.routes}
			eng := runtime.New(wf, st, chainExecutor{chain: chain},
				runtime.WithEventObserver(usage.observe),
				runtime.WithRecoveryDispatch(recovery.Dispatch(recovery.DefaultRecipes())),
				runtime.WithWorkDir(t.TempDir()))
			execErr := eng.Run(ctx, "run-1", map[string]any{})
			var rt *runtime.RuntimeError
			if !errors.As(execErr, &rt) || rt.Code != runtime.ErrCodeUsageLimitBlocked {
				t.Fatalf("run ended on %v, want the chain's USAGE_LIMIT_BLOCKED — the premise of this witness", execErr)
			}

			h.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-1", CredPlan: plan}, usage, execErr, false)

			p, err := h.pledges.Get(ctx, donorPledgeID())
			if err != nil {
				t.Fatalf("pledge: %v", err)
			}
			if tc.wantDead {
				if p.ConsecutiveAuthFailures == 0 {
					t.Fatalf("auth failures = 0 (cooldown until %v) — the donor's own route met the dead credential, and resting it on the other route's window hands it back still dead", p.CooldownUntil)
				}
				return
			}
			if p.ConsecutiveAuthFailures != 0 {
				t.Fatalf("auth failures = %d — the donor's subscription met a usage window; the dead credential was the team's z.ai key, another route's", p.ConsecutiveAuthFailures)
			}
			// The broker pads the reset by its come-back minute.
			if want := reset.Add(time.Minute); p.CooldownUntil == nil || !p.CooldownUntil.Equal(want) {
				t.Fatalf("cooldown = %v, want the donor rested until just past its own window's reset at %s", p.CooldownUntil, reset.Format(time.RFC3339))
			}
		})
	}
}

// chainExecutor fails every node on one exhausted chain.
type chainExecutor struct{ chain *model.ErrChainExhausted }

func (e chainExecutor) Execute(context.Context, ir.Node, map[string]any) (map[string]any, error) {
	return nil, fmt.Errorf("model: node %q: %w", "agent", e.chain)
}

// A gateway route whose call reports no credential keys (a backend whose arm
// has not landed, a verdict whose keys never reached the walk) is placed the
// way its spend joins the plan: on the row the plan names for the node's
// gateway route — the walk records one only when a sealed key funds the
// gateway (ADR-131 §5) — and on nobody when it names none, the environment
// funding the route. In both orders of [the gateway route → dead, the team's
// subscription → a five-hour window]:
//
//   - the plan names the lease for the gateway: the lease is the dead
//     credential. Placed on nobody, it took the table's ConditionOK and its
//     auth-failure count was cleared, a dead key kept in rotation;
//   - the plan names no gateway row: the dead gateway is the environment's,
//     and the lease the plan names for the subscription rests on its window.
//
// A label that says the call rode the environment outranks the plan's
// prediction: a claude_code session on the pod's ambient anthropic env that
// was refused is nobody's, even where the plan names the lease for anthropic
// — the lease met nothing, and the other route's window is the team key's.
func TestRecordPoolSpend_aKeylessGatewayVerdictIsTheLeaseThePlanNames(t *testing.T) {
	reset := time.Now().UTC().Add(5 * time.Hour).Truncate(time.Second)
	gateway := model.ChainRoute{Backend: delegate.BackendClaw, Model: "openai_compatible/qwen3", Provider: "openai_compatible"}
	subscription := model.ChainRoute{Backend: delegate.BackendClaudeCode, Model: "claude-sonnet-4-5", Source: "anthropic-oauth"}
	dead := &delegate.ErrAuthFailed{Provider: "openai_compatible", Detail: "401 invalid api key"}
	window := &delegate.ErrRateLimited{Provider: "anthropic", Kind: delegate.RateLimitKindUsageWindow,
		Detail: "Claude usage limit reached", ResetAt: reset}
	ambient := model.ChainRoute{Backend: delegate.BackendClaudeCode, Model: "claude-sonnet-4-5", Provider: "anthropic", Source: "anthropic-env"}
	teamKey := model.ChainRoute{Backend: delegate.BackendClaw, Model: "zai/glm-5.3", Provider: "zai",
		Credential: credroute.Spend{Channel: credroute.ChannelRunKey, Slot: "zai", Fingerprint: "fp-team-zai"}}
	for _, tc := range []struct {
		name     string
		plan     map[string]string
		routes   []model.ChainRoute
		errs     []error
		wantDead bool
		// wantUntouched: the lease met nothing — no auth failure, no rest.
		wantUntouched bool
	}{
		{"ambient session, ambient first", map[string]string{"agent|anthropic": donorFingerprint},
			[]model.ChainRoute{ambient, teamKey}, []error{dead, window}, false, true},
		{"ambient session, team key first", map[string]string{"agent|anthropic": donorFingerprint},
			[]model.ChainRoute{teamKey, ambient}, []error{window, dead}, false, true},
		{"lease on the gateway, gateway first", map[string]string{"agent|openai_compatible": donorFingerprint, "agent|anthropic": "fp-team-anthropic"},
			[]model.ChainRoute{gateway, subscription}, []error{dead, window}, true, false},
		{"lease on the gateway, subscription first", map[string]string{"agent|openai_compatible": donorFingerprint, "agent|anthropic": "fp-team-anthropic"},
			[]model.ChainRoute{subscription, gateway}, []error{window, dead}, true, false},
		{"env-funded gateway, gateway first", map[string]string{"agent|anthropic": donorFingerprint},
			[]model.ChainRoute{gateway, subscription}, []error{dead, window}, false, false},
		{"env-funded gateway, subscription first", map[string]string{"agent|anthropic": donorFingerprint},
			[]model.ChainRoute{subscription, gateway}, []error{window, dead}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPoolHarness(t, credpool.Limits{MaxUSDPerDay: 10})
			ctx := context.Background()
			usage := newMetricsEmitter(&recordingEmitter{}, metrics.New(), tc.plan, nil)
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatalf("store: %v", err)
			}
			wf := &ir.Workflow{
				Name:  "gateway_route_verdict",
				Entry: "agent",
				Nodes: map[string]ir.Node{
					"agent": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "agent"}},
					"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
				},
				Edges: []*ir.Edge{{From: "agent", To: "done"}},
			}
			chain := &model.ErrChainExhausted{Chain: "routes", Errs: tc.errs, Routes: tc.routes}
			eng := runtime.New(wf, st, chainExecutor{chain: chain},
				runtime.WithEventObserver(usage.observe),
				runtime.WithRecoveryDispatch(recovery.Dispatch(recovery.DefaultRecipes())),
				runtime.WithWorkDir(t.TempDir()))
			execErr := eng.Run(ctx, "run-1", map[string]any{})
			var rt *runtime.RuntimeError
			if !errors.As(execErr, &rt) || rt.Code != runtime.ErrCodeUsageLimitBlocked {
				t.Fatalf("run ended on %v, want the chain's USAGE_LIMIT_BLOCKED — the premise of this witness", execErr)
			}

			h.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-1", CredPlan: tc.plan}, usage, execErr, false)

			p, err := h.pledges.Get(ctx, donorPledgeID())
			if err != nil {
				t.Fatalf("pledge: %v", err)
			}
			if tc.wantDead {
				if p.ConsecutiveAuthFailures == 0 {
					t.Fatalf("auth failures = 0 (cooldown until %v) — the plan names this lease for the gateway route whose credential was refused; placed on nobody, the dead key stays in rotation", p.CooldownUntil)
				}
				return
			}
			if p.ConsecutiveAuthFailures != 0 {
				t.Fatalf("auth failures = %d — the refused route was the environment's (an env-funded gateway, an ambient session); the lease met no rejection", p.ConsecutiveAuthFailures)
			}
			if tc.wantUntouched {
				if p.CooldownUntil != nil {
					t.Fatalf("cooldown = %v, want none — the window was the team key's, and the lease met nothing", p.CooldownUntil)
				}
				return
			}
			if want := reset.Add(time.Minute); p.CooldownUntil == nil || !p.CooldownUntil.Equal(want) {
				t.Fatalf("cooldown = %v, want the lease rested until just past its own window's reset at %s", p.CooldownUntil, reset.Format(time.RFC3339))
			}
		})
	}
}
