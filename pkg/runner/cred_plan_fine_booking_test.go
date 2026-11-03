package runner

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The fine door-lease booking, joined END TO END (#2288, revi R30d246's
// fence): the PRODUCER half derives the plan from the walk and the sealed
// bundle (model.CredentialPlan — the function the publisher stamps), the
// RUNNER half groups the attempt's spend by it through the production
// event hooks, and the BROKER half books the slice that matches the
// lease's own fingerprint. One fingerprint space, three halves; a test
// that fed each half its own space is the exact gap R30d246 caught.

// planKnownProviders is the walk's provider vocabulary for these tests.
var planKnownProviders = map[string]bool{
	"anthropic": true,
	"openai":    true,
	"zai":       true,
}

// fineBookingWorkflow is a run the pool serves whole-bundle: the donor's
// lent Claude subscription funds the impl node, the team's own openai key
// funds the review node. Two credentials, one attempt — the split a
// whole-attempt booking books on the donor alone.
func fineBookingWorkflow() *ir.Workflow {
	return &ir.Workflow{Nodes: map[string]ir.Node{
		"impl":   agentNodeForPlan("impl", "anthropic", "anthropic/claude-opus-5"),
		"review": agentNodeForPlan("review", "openai", "openai/gpt-5"),
	}}
}

func agentNodeForPlan(id, provider, mdl string) *ir.AgentNode {
	return &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: id},
		LLMFields: ir.LLMFields{Provider: provider, Model: mdl},
	}
}

// fineBookingBundle is the sealed bundle that scenario seals: the donor's
// subscription (the pool-sourced slot) and the team's own openai key,
// each with the server-stamped audit fingerprint the lease carries too.
func fineBookingBundle() secrets.RunBundle {
	return secrets.RunBundle{
		APIKeys: map[secrets.Provider]string{
			secrets.ProviderOpenAI: "sk-own-openai",
		},
		APIKeyFingerprints: map[secrets.Provider]string{
			secrets.ProviderOpenAI: "fp-own-openai",
		},
		OAuthCredentials: map[string][]byte{
			string(secrets.OAuthKindClaudeCode): []byte("payload"),
		},
		OAuthFingerprints: map[string]string{
			string(secrets.OAuthKindClaudeCode): donorFingerprint,
		},
		PoolSourced: map[string]bool{string(secrets.OAuthKindClaudeCode): true},
	}
}

// donorFingerprint is what the lent subscription's record stamps — the
// seed the pool harness gives the donor's OAuth record, so the lease and
// the plan literally share the string.
const donorFingerprint = "fp-donor-oauth"

// donorPledgeID is the pledge the pool harness seeds: one donor lending
// their Claude subscription.
func donorPledgeID() string {
	return credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code")
}

// credpoolLimitsForFine gives the donor a spend ceiling the tests can
// measure against ($10/day) and room for the one run each holds.
func credpoolLimitsForFine() credpool.Limits {
	return credpool.Limits{MaxUSDPerDay: 10, MaxConcurrentRuns: 1}
}

// finePlan runs the PRODUCER function the publisher stamps and returns
// the wire map. Fails before the booking assertions when the producer
// itself drifted — this test is the join, not each side in isolation.
func finePlan(t *testing.T) map[string]string {
	t.Helper()
	res := model.EffectiveProviders(model.RouteView{Workflow: fineBookingWorkflow()}, planKnownProviders)
	if !res.NarrowSafe {
		t.Fatalf("the fine-booking walk widened — test premise broken: %+v", res)
	}
	plan := model.CredentialPlan(res, fineBookingBundle())
	want := map[string]string{
		"impl|anthropic": donorFingerprint,
		"review|openai":  "fp-own-openai",
	}
	if !maps.Equal(plan, want) {
		t.Fatalf("the producer's plan = %v, want %v — the plan must name the lease's own fingerprint for the donor's node", plan, want)
	}
	return plan
}

// fineBookingUsage drives the PRODUCTION event hooks the executor fires,
// so the fine table is the one the real chain produces: impl spent $2 on
// the donor's subscription, review spent $8 on the team's own key.
func fineBookingUsage(t *testing.T, plan map[string]string) *metricsEmitter {
	t.Helper()
	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New(), plan)
	hooks := model.NewStoreEventHooks(
		context.Background(), usage, "run-1", iterlog.New(iterlog.LevelError, nil), nil, nil,
	)
	hooks.OnDelegateFinished("impl", model.DelegateInfo{
		BackendName: "claude_code", Provider: "anthropic", Tokens: 900, CostUSD: 2,
	})
	hooks.OnDelegateFinished("review", model.DelegateInfo{
		BackendName: "codex", Provider: "openai", Tokens: 100, CostUSD: 8,
	})
	return usage
}

// TestRecordPoolSpend_theFinePlanBooksOnlyTheDonorsSlice: a plan-stamped
// run reports the per-credential table, and the donor's ledger records
// the slice that ran on THEIR subscription — two dollars of the ten the
// attempt spent. The whole-attempt booking would charge them all ten.
//
// Mutant guards: revert the runner's flip (report no table) and this
// books $10 — red; stamp a plan whose donor value is any string but the
// lease's fingerprint and this books $0 — red; break the producer's
// slot reading and the want-map above reds before any booking happens.
func TestRecordPoolSpend_theFinePlanBooksOnlyTheDonorsSlice(t *testing.T) {
	h := newPoolHarnessForRun(t, credpoolLimitsForFine(), "run-1")
	ctx := context.Background()

	lease, err := h.leases.GetOpenByRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("the seeded lease: %v", err)
	}
	if lease.Fingerprint != donorFingerprint {
		t.Fatalf("the lease carries fingerprint %q, want %q — the consumer half has nothing to slice by", lease.Fingerprint, donorFingerprint)
	}

	msg := &queue.RunMessage{RunID: "run-1", TenantID: "team-1", CredPlan: finePlan(t)}
	h.runner.recordPoolSpend(msg, fineBookingUsage(t, msg.CredPlan), nil, false)

	day, _, err := h.ledger.Usage(ctx, donorPledgeID(), time.Now().UTC())
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if day.CostUSD != 2 {
		t.Fatalf("the donor's ledger = $%.2f, want $2 — the fine table books only the lease credential's slice, never the whole attempt", day.CostUSD)
	}
	if _, openErr := h.leases.GetOpenByRun(ctx, "run-1"); !errors.Is(openErr, credpool.ErrNotFound) {
		t.Fatalf("the lease is still open after the settling report (err=%v) — it must be closed", openErr)
	}
}

// TestRecordPoolSpend_noPlanKeepsTheCoarseBooking: the same attempt,
// published without a plan (an older publisher, or a walk that widened),
// keeps the whole-attempt booking. The nil-vs-present distinction is the
// contract — silently flipping the default to fine would book the donor
// zero on exactly the runs whose routes the publisher could not read.
func TestRecordPoolSpend_noPlanKeepsTheCoarseBooking(t *testing.T) {
	h := newPoolHarnessForRun(t, credpoolLimitsForFine(), "run-1")

	h.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-1"},
		fineBookingUsage(t, nil), nil, false)

	day, _, err := h.ledger.Usage(context.Background(), donorPledgeID(), time.Now().UTC())
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if day.CostUSD != 10 {
		t.Fatalf("the donor's ledger = $%.2f, want $10 — a plan-less publication books the whole attempt, as before the producer existed", day.CostUSD)
	}
}

// TestRecordPoolSpend_aPlanWhoseDoorNeverFiredBooksZero: the run carried
// the lease but spent only its own key — the fine table names no spend on
// the donor's fingerprint, and the ledger stays at zero instead of
// booking the attempt's whole spend on them.
func TestRecordPoolSpend_aPlanWhoseDoorNeverFiredBooksZero(t *testing.T) {
	h := newPoolHarnessForRun(t, credpoolLimitsForFine(), "run-1")
	plan := finePlan(t)

	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New(), plan)
	hooks := model.NewStoreEventHooks(
		context.Background(), usage, "run-1", iterlog.New(iterlog.LevelError, nil), nil, nil,
	)
	hooks.OnDelegateFinished("review", model.DelegateInfo{
		BackendName: "codex", Provider: "openai", Tokens: 700, CostUSD: 7,
	})

	h.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-1", CredPlan: plan}, usage, nil, false)

	day, _, err := h.ledger.Usage(context.Background(), donorPledgeID(), time.Now().UTC())
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if day.CostUSD != 0 {
		t.Fatalf("the donor's ledger = $%.2f, want $0 — the door never ran on their credential", day.CostUSD)
	}
}

// clawHintPlan is the plan for the finding's shape: the impl node rides
// CLAW with a `provider: zai` hint over an `anthropic/…` spec. The hint
// names a route claw never spends (the registry funds the prefix), so the
// walk must carry the prefix's slot or every step observation the loop
// produces joins a key the plan never stamped — the donor's $0 harm,
// silent, on a plan the publisher DID stamp.
func clawHintPlan(t *testing.T) map[string]string {
	t.Helper()
	wf := fineBookingWorkflow()
	impl := wf.Nodes["impl"].(*ir.AgentNode)
	impl.LLMFields.Backend = delegate.BackendClaw
	impl.LLMFields.Provider = "zai"
	res := model.EffectiveProviders(model.RouteView{Workflow: wf}, planKnownProviders)
	if !res.NarrowSafe {
		t.Fatalf("the hint-steered claw walk widened — test premise broken: %+v", res)
	}
	if got := res.Sites["impl"]; !slices.Equal(got, []string{"zai", "anthropic"}) {
		t.Fatalf("Sites[impl] = %v, want [zai anthropic] — the hint names the route, the prefix names the slot claw funds", got)
	}
	plan := model.CredentialPlan(res, fineBookingBundle())
	want := map[string]string{
		"impl|anthropic": donorFingerprint,
		"review|openai":  "fp-own-openai",
	}
	if !maps.Equal(plan, want) {
		t.Fatalf("plan = %v, want %v — the prefix's slot plans the donor's fingerprint (the zai hint seals nothing)", plan, want)
	}
	return plan
}

// clawStepUsage drives the PRODUCTION hooks the way a priced claw loop
// fires them: one llm_request naming the serving spec, steps carrying the
// loop's own provider word — "anthropic", the word servingProvider answers
// for this hint+spec pair (pinned by the model package's servingProvider
// table, pkg/backend/model/cred_plan_claw_slot_test.go) — then the
// aggregate closing the node, which the summarised guard must skip or the
// donor books twice.
func clawStepUsage(t *testing.T, plan map[string]string) (*metricsEmitter, float64) {
	t.Helper()
	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New(), plan)
	hooks := model.NewStoreEventHooks(
		context.Background(), usage, "run-1", iterlog.New(iterlog.LevelError, nil), nil, nil,
	)
	hooks.OnLLMRequest("impl", model.LLMRequestInfo{Model: "anthropic/claude-opus-5", WireModel: "claude-opus-5"})
	hooks.OnLLMStepFinish("impl", model.LLMStepInfo{
		Number: 1, InputTokens: 20000, OutputTokens: 2000, Provider: "anthropic",
	})
	costAfterSteps, _, _, _ := usage.RunTotals()
	if costAfterSteps <= 0 {
		t.Fatalf("the steps priced nothing ($%.4f) — the test premise (priced steps) is broken", costAfterSteps)
	}
	hooks.OnDelegateFinished("impl", model.DelegateInfo{
		BackendName: "claw", Provider: "anthropic", Tokens: 900, CostUSD: 2,
	})
	costAfterAggregate, _, _, _ := usage.RunTotals()
	if costAfterAggregate != costAfterSteps {
		t.Fatalf("the summarised guard let the aggregate double-book: $%.4f after steps, $%.4f after it", costAfterSteps, costAfterAggregate)
	}
	hooks.OnDelegateFinished("review", model.DelegateInfo{
		BackendName: "codex", Provider: "openai", Tokens: 100, CostUSD: 8,
	})
	return usage, costAfterSteps
}

// TestRecordPoolSpend_aHintSteeredClawLoopBooksTheDonorsSliceFromItsSteps:
// the finding's shape, end to end. A claw loop whose steps were priced
// leaves the STEPS as the only observations (the summarised guard skips
// the aggregate), and those steps book the donor's slice through the
// provider word the loop stamps. The observations hand-stamp that word
// (clawStepUsage), so a mutant of the STAMPING itself — servingProvider's
// claw prefix-over-hint reverted — leaves this test green; the guards
// that red on it live in pkg/backend/model/cred_plan_claw_slot_test.go
// (the servingProvider table and the Execute wiring witness). Mutants
// this test does red: the walk dropping the prefix's slot (clawHintPlan's
// Sites/plan assertions fail before any booking — in production the harm
// is the donor's silent $0); the summarised guard lifting (the aggregate
// double-books at the check in clawStepUsage); a fine-table join that
// books the priced steps on any fingerprint but the lease's (the ledger
// read-back misses).
func TestRecordPoolSpend_aHintSteeredClawLoopBooksTheDonorsSliceFromItsSteps(t *testing.T) {
	h := newPoolHarnessForRun(t, credpoolLimitsForFine(), "run-1")
	ctx := context.Background()

	plan := clawHintPlan(t)
	usage, stepsCost := clawStepUsage(t, plan)

	h.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-1", CredPlan: plan}, usage, nil, false)

	day, _, err := h.ledger.Usage(ctx, donorPledgeID(), time.Now().UTC())
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	// The ledger keeps money at milli-cent resolution (CostToMillis), so
	// the read-back is the booked slice rounded to it.
	if math.Abs(day.CostUSD-stepsCost) >= 1e-3 {
		t.Fatalf("the donor's ledger = $%.6f, want $%.6f — the steps that ran on their lent subscription book through the loop's provider word", day.CostUSD, stepsCost)
	}
	if _, openErr := h.leases.GetOpenByRun(ctx, "run-1"); !errors.Is(openErr, credpool.ErrNotFound) {
		t.Fatalf("the lease is still open after the settling report (err=%v) — it must be closed", openErr)
	}
}

// TestRecordPoolSpend_anEmptyFineTableUnderSpendWarns: a stamped plan
// whose fine table came back EMPTY while the attempt spent is a join
// drift, and the broker's $0 booking would otherwise be silent — the
// report carries one warn naming the run and the spend. A table with rows
// and a plan-less publication (the coarse contract) stay quiet.
func TestRecordPoolSpend_anEmptyFineTableUnderSpendWarns(t *testing.T) {
	var buf bytes.Buffer
	newLoggedRunner := func(t *testing.T, runID string) *poolHarness {
		t.Helper()
		h := newPoolHarnessForRun(t, credpoolLimitsForFine(), runID)
		h.runner.cfg.Logger = iterlog.New(iterlog.LevelWarn, &buf)
		return h
	}

	// The drift: the plan promises per-credential answers, the observations
	// hit none of its keys, the attempt still spent.
	h := newLoggedRunner(t, "run-1")
	plan := finePlan(t)
	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New(), plan)
	hooks := model.NewStoreEventHooks(
		context.Background(), usage, "run-1", iterlog.New(iterlog.LevelError, nil), nil, nil,
	)
	hooks.OnDelegateFinished("ghost", model.DelegateInfo{
		BackendName: "claw", Provider: "anthropic", Tokens: 100, CostUSD: 3,
	})
	h.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-1", CredPlan: plan}, usage, nil, false)
	if !strings.Contains(buf.String(), "EMPTY fine table") || !strings.Contains(buf.String(), "run-1") {
		t.Fatalf("the report logged %q, want the empty-table warn naming the run", buf.String())
	}

	// Control 1: the table carries rows (the door fired, or the team's own
	// key spent) — no line.
	buf.Reset()
	h2 := newLoggedRunner(t, "run-2")
	h2.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-2", TenantID: "team-1", CredPlan: finePlan(t)},
		fineBookingUsage(t, finePlan(t)), nil, false)
	if buf.Len() != 0 {
		t.Fatalf("a table with rows logged %q, want silence", buf.String())
	}

	// Control 2: no plan at all — the coarse contract, not a drift.
	buf.Reset()
	h3 := newLoggedRunner(t, "run-3")
	h3.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-3", TenantID: "team-1"},
		fineBookingUsage(t, nil), nil, false)
	if buf.Len() != 0 {
		t.Fatalf("a plan-less publication logged %q, want silence", buf.String())
	}
}

// recoveryRungPlan is the plan for the fired-rung shape: a Verified Action
// whose rung 4 dispatches a synthetic agent under `heal__recover`, the id
// the recovery dispatch's own events carry. The walk reads the rung at
// `heal#agent_recovery`; the plan must key the rung's row on the runtime id
// or the fired recovery joins nothing.
func recoveryRungPlan(t *testing.T) map[string]string {
	t.Helper()
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"heal": &ir.ToolNode{
			BaseNode:      ir.BaseNode{ID: "heal"},
			Command:       "make build",
			Goal:          "the build passes",
			Postcondition: "test -f built",
			Policy:        ir.PolicyRecover,
			Recovery:      &ir.RecoverySpec{MaxAgentAttempts: 1, Model: "anthropic/claude-opus-5"},
		},
	}}
	res := model.EffectiveProviders(model.RouteView{Workflow: wf}, planKnownProviders)
	if !res.NarrowSafe {
		t.Fatalf("the rung walk widened — test premise broken: %+v", res)
	}
	plan := model.CredentialPlan(res, fineBookingBundle())
	want := map[string]string{
		"heal|anthropic":          donorFingerprint,
		"heal__recover|anthropic": donorFingerprint,
	}
	if !maps.Equal(plan, want) {
		t.Fatalf("plan = %v, want %v — the rung keys under the runtime id, funded by the donor's lent subscription", plan, want)
	}
	return plan
}

// TestRecordPoolSpend_aFiredAgentRecoveryBooksTheDonorsSlice: rung 4
// dispatched, its delegate event carrying `heal__recover` — the recovery's
// spend books the donor's slice through the plan's runtime-id row. Mutant
// guards: the derivation dropping the runtime-id row reds (the join misses,
// the donor books $0 under real spend and the empty-table telltale fires);
// the site mapper keying the rung on the tool node alone reds the same way.
func TestRecordPoolSpend_aFiredAgentRecoveryBooksTheDonorsSlice(t *testing.T) {
	h := newPoolHarnessForRun(t, credpoolLimitsForFine(), "run-1")
	ctx := context.Background()

	plan := recoveryRungPlan(t)
	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New(), plan)
	hooks := model.NewStoreEventHooks(
		context.Background(), usage, "run-1", iterlog.New(iterlog.LevelError, nil), nil, nil,
	)
	hooks.OnDelegateFinished("heal__recover", model.DelegateInfo{
		BackendName: "claw", Provider: "anthropic", Tokens: 400, CostUSD: 5,
	})

	h.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-1", CredPlan: plan}, usage, nil, false)

	day, _, err := h.ledger.Usage(ctx, donorPledgeID(), time.Now().UTC())
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if day.CostUSD != 5 {
		t.Fatalf("the donor's ledger = $%.2f, want $5 — the recovery spend ran on their lent subscription and must book", day.CostUSD)
	}
	if _, openErr := h.leases.GetOpenByRun(ctx, "run-1"); !errors.Is(openErr, credpool.ErrNotFound) {
		t.Fatalf("the lease is still open after the settling report (err=%v) — it must be closed", openErr)
	}
}

// TestRecordPoolSpend_gatewayFundedSpendKeepsTheEmptyTableQuiet: the whole
// attempt's spend rode the openai_compatible gateway — a credential the
// runner's environment holds, which no plan row ever names on purpose. The
// empty table is CORRECT here, and the join-drift warn must stay silent or
// every gateway-served lease run trains its reader to ignore the one warn
// meant to surface real drift. Mutant guards: dropping the env-funded
// exclusion reds (the gateway observation counts as a miss, the warn
// fires); gating the warn on the whole-attempt cost alone reds the same.
func TestRecordPoolSpend_gatewayFundedSpendKeepsTheEmptyTableQuiet(t *testing.T) {
	var buf bytes.Buffer
	h := newPoolHarnessForRun(t, credpoolLimitsForFine(), "run-1")
	h.runner.cfg.Logger = iterlog.New(iterlog.LevelWarn, &buf)

	plan := finePlan(t)
	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New(), plan)
	hooks := model.NewStoreEventHooks(
		context.Background(), usage, "run-1", iterlog.New(iterlog.LevelError, nil), nil, nil,
	)
	hooks.OnLLMRequest("review", model.LLMRequestInfo{Model: "openai_compatible/gpt-5", WireModel: "gpt-5"})
	hooks.OnDelegateFinished("review", model.DelegateInfo{
		BackendName: "claw", Tokens: 900, CostUSD: 4,
	})

	h.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-1", CredPlan: plan}, usage, nil, false)
	if buf.Len() != 0 {
		t.Fatalf("a gateway-funded attempt logged %q, want silence — the plan names no row for env-funded spend on purpose", buf.String())
	}

	// The other env-funded shape: a claude_code session that rode the pod's
	// ambient anthropic env (its own label says so). Its credential is the
	// deployment's, never a bundle row — the emptiness is correct too.
	buf.Reset()
	hEnv := newPoolHarnessForRun(t, credpoolLimitsForFine(), "run-4")
	hEnv.runner.cfg.Logger = iterlog.New(iterlog.LevelWarn, &buf)
	usageEnv := newMetricsEmitter(&recordingEmitter{}, metrics.New(), plan)
	hooksEnv := model.NewStoreEventHooks(
		context.Background(), usageEnv, "run-4", iterlog.New(iterlog.LevelError, nil), nil, nil,
	)
	hooksEnv.OnDelegateFinished("impl", model.DelegateInfo{
		BackendName: "claude_code", Fingerprint: "anthropic-env", Tokens: 900, CostUSD: 3,
	})
	hEnv.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-4", TenantID: "team-1", CredPlan: plan}, usageEnv, nil, false)
	if buf.Len() != 0 {
		t.Fatalf("an ambient-env attempt logged %q, want silence — the session's own label names no bundle credential", buf.String())
	}

	// The same shape on a route a plan is supposed to answer still warns:
	// the exclusion is env-funded routes, not the warn itself. The impl
	// node's route naming openai joins a key the plan never stamped for it
	// (impl plans the donor's anthropic) — vendor-funded drift.
	buf.Reset()
	h2 := newPoolHarnessForRun(t, credpoolLimitsForFine(), "run-2")
	h2.runner.cfg.Logger = iterlog.New(iterlog.LevelWarn, &buf)
	usage2 := newMetricsEmitter(&recordingEmitter{}, metrics.New(), plan)
	hooks2 := model.NewStoreEventHooks(
		context.Background(), usage2, "run-2", iterlog.New(iterlog.LevelError, nil), nil, nil,
	)
	hooks2.OnLLMRequest("impl", model.LLMRequestInfo{Model: "openai/gpt-5", WireModel: "gpt-5"})
	hooks2.OnDelegateFinished("impl", model.DelegateInfo{
		BackendName: "claw", Tokens: 900, CostUSD: 4,
	})
	h2.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-2", TenantID: "team-1", CredPlan: plan}, usage2, nil, false)
	if !strings.Contains(buf.String(), "EMPTY fine table") {
		t.Fatalf("a vendor-funded miss logged %q, want the empty-table warn — this one is drift", buf.String())
	}
}

// TestSupervisorUsageSink_JoinsThePlanLikeAnEvent: the OnUsage sink's
// observation joins the credential plan the way the event paths do — the
// source IS the node half the walk recorded (`supervisor:<name>`), so the
// sink's spend books its row, and an observation the plan names nothing
// for counts as a miss the report warns about. A sink path outside the
// fine table would book the lease $0 under real spend with the telltale
// quiet. Mutant guards: the fold dropped from addRoute reds both halves;
// the plan join reading the sink's source as a label reds the first.
func TestSupervisorUsageSink_JoinsThePlanLikeAnEvent(t *testing.T) {
	// The plan names the supervisor's site: its evaluation books the
	// donor's slice, the same space the lease stamps.
	plan := map[string]string{"supervisor:pacer|anthropic": donorFingerprint}
	m := newMetricsEmitter(&recordingEmitter{}, metrics.New(), plan)
	m.addRoute(delegate.BackendClaw, "anthropic/claude-opus-5", "supervisor:pacer", 0, 900, 100, 1000)
	totals := m.FingerprintSpendTotals()
	row, ok := totals[donorFingerprint]
	if !ok {
		t.Fatalf("the sink booked no fine row; table: %v — a supervisor evaluation must join the plan like an event", totals)
	}
	if row.AggregateTokens != 1000 {
		t.Fatalf("the donor's fine row = %d aggregate tokens, want 1000", row.AggregateTokens)
	}
	if _, _, missObs := m.MissedPlanSpend(); missObs != 0 {
		t.Fatalf("a joined observation counted %d miss(es), want 0", missObs)
	}

	// The same sink naming a provider the plan carries no row for on the
	// supervisor's site counts as a miss — the report's under-book warn
	// reads it, the coarse booking keeps the donor honest. The miss carries
	// the route table's own token total (the sink books both directions AND
	// their sum, its convention since #2721).
	m2 := newMetricsEmitter(&recordingEmitter{}, metrics.New(), plan)
	m2.addRoute(delegate.BackendClaw, "openai/gpt-5", "supervisor:pacer", 0, 10, 5, 15)
	if _, tokens, missObs := m2.MissedPlanSpend(); missObs != 1 || tokens != 30 {
		t.Fatalf("an unjoined sink observation counted (misses=%d, tokens=%d), want (1, 30)", missObs, tokens)
	}
}

// TestRecordPoolSpend_aRuntimeNodeTheWalkCannotSeeWarnsAndBooksNobody: a
// claw subagent loop's events carry the `subagent:<agent>` id the walk
// never saw — runtime input the plan cannot cover — so its spend books
// nobody even with the table's other rows full. The under-booking must
// surface at the report (one warn naming the missed slice) while the
// broker still books only the matched slices, never the donor by guess.
// Mutant guards: dropping the miss accounting reds (no warn); dropping the
// partial-miss branch reds (a non-empty table stays silent); booking the
// missed slice on any row reds the ledger read.
func TestRecordPoolSpend_aRuntimeNodeTheWalkCannotSeeWarnsAndBooksNobody(t *testing.T) {
	var buf bytes.Buffer
	newLoggedRunner := func(t *testing.T, runID string) *poolHarness {
		t.Helper()
		h := newPoolHarnessForRun(t, credpoolLimitsForFine(), runID)
		h.runner.cfg.Logger = iterlog.New(iterlog.LevelWarn, &buf)
		return h
	}

	h := newLoggedRunner(t, "run-1")
	plan := finePlan(t)
	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New(), plan)
	hooks := model.NewStoreEventHooks(
		context.Background(), usage, "run-1", iterlog.New(iterlog.LevelError, nil), nil, nil,
	)
	// The door fired: the impl node booked the donor's slice ($2).
	hooks.OnDelegateFinished("impl", model.DelegateInfo{
		BackendName: "claude_code", Provider: "anthropic", Tokens: 900, CostUSD: 2,
	})
	// A claw subagent spent on a provider the plan carries no row for on
	// any node — the Task tool's runtime model arg, invisible at launch.
	hooks.OnLLMRequest("subagent:auditor", model.LLMRequestInfo{Model: "openai/gpt-5", WireModel: "gpt-5"})
	hooks.OnLLMStepFinish("subagent:auditor", model.LLMStepInfo{
		Number: 1, InputTokens: 20000, OutputTokens: 2000, Provider: "openai",
	})

	h.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-1", CredPlan: plan}, usage, nil, false)
	if !strings.Contains(buf.String(), "under-books") || !strings.Contains(buf.String(), "run-1") {
		t.Fatalf("the report logged %q, want the under-booked-slice warn naming the run", buf.String())
	}

	day, _, err := h.ledger.Usage(context.Background(), donorPledgeID(), time.Now().UTC())
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if day.CostUSD != 2 {
		t.Fatalf("the donor's ledger = $%.2f, want $2 — the missed slice books nobody, never the donor by guess", day.CostUSD)
	}

	// Control: the same attempt without the runtime node — every
	// non-env observation joined a row, no line.
	buf.Reset()
	h2 := newLoggedRunner(t, "run-2")
	h2.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-2", TenantID: "team-1", CredPlan: finePlan(t)},
		fineBookingUsage(t, finePlan(t)), nil, false)
	if buf.Len() != 0 {
		t.Fatalf("a fully-joined attempt logged %q, want silence", buf.String())
	}
}
