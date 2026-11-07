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
	"github.com/SocialGouv/iterion/pkg/supervise"
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
	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New(), plan, nil)
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

	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New(), plan, nil)
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
// produces joins a key the plan never stamped — a miss on every step, the
// fine booking lost on a plan the publisher DID stamp.
func clawHintPlan(t *testing.T) map[string]string {
	t.Helper()
	wf := fineBookingWorkflow()
	impl := wf.Nodes["impl"].(*ir.AgentNode)
	impl.Backend = delegate.BackendClaw
	impl.Provider = "zai"
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
	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New(), plan, nil)
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
// Sites/plan assertions fail before any booking — in production the steps
// would miss, void the fine table and book the donor the whole attempt);
// the summarised guard lifting (the aggregate double-books at the check in
// clawStepUsage); a fine-table join that books the priced steps on any
// fingerprint but the lease's (the ledger read-back misses).
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

// voidedTableWarn is the one line a report writes when a miss voids its
// fine table.
const voidedTableWarn = "no fine table rides, the lease books the whole attempt"

// TestRecordPoolSpend_aWhollyUnjoinedAttemptBooksTheWholeAttempt: a stamped
// plan whose keys answered nothing while the attempt spent is a join drift.
// An empty table would book the lease $0 for spend that may have run on its
// credential, so the report sends NO table — the coarse whole-attempt
// booking, the donor charged the $3 — and one warn names the run. A table
// every observation joined and a plan-less publication (the coarse
// contract) stay quiet. Mutant guards: dropping the void reds the ledger
// read ($0); dropping the warn reds the log assertion.
func TestRecordPoolSpend_aWhollyUnjoinedAttemptBooksTheWholeAttempt(t *testing.T) {
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
	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New(), plan, nil)
	hooks := model.NewStoreEventHooks(
		context.Background(), usage, "run-1", iterlog.New(iterlog.LevelError, nil), nil, nil,
	)
	hooks.OnDelegateFinished("ghost", model.DelegateInfo{
		BackendName: "claw", Provider: "anthropic", Tokens: 100, CostUSD: 3,
	})
	h.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-1", CredPlan: plan}, usage, nil, false)
	if !strings.Contains(buf.String(), voidedTableWarn) || !strings.Contains(buf.String(), "run-1") {
		t.Fatalf("the report logged %q, want the voided-table warn naming the run", buf.String())
	}
	day, _, err := h.ledger.Usage(context.Background(), donorPledgeID(), time.Now().UTC())
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if day.CostUSD != 3 {
		t.Fatalf("the donor's ledger = $%.2f, want $3 — an attempt whose spend joined no plan key books whole, never $0 for spend that may have run on the lease's credential", day.CostUSD)
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
// spend books the donor's slice through the plan's runtime-id row, the
// table riding with no miss. Mutant guards: the derivation dropping the
// runtime-id row, or the site mapper keying the rung on the tool node
// alone, reds at recoveryRungPlan's want-map before any booking (in
// production the join would miss and void the run's fine table); the warn
// assertion below reds on any miss that slips past the want-map.
func TestRecordPoolSpend_aFiredAgentRecoveryBooksTheDonorsSlice(t *testing.T) {
	var buf bytes.Buffer
	h := newPoolHarnessForRun(t, credpoolLimitsForFine(), "run-1")
	h.runner.cfg.Logger = iterlog.New(iterlog.LevelWarn, &buf)
	ctx := context.Background()

	plan := recoveryRungPlan(t)
	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New(), plan, nil)
	hooks := model.NewStoreEventHooks(
		context.Background(), usage, "run-1", iterlog.New(iterlog.LevelError, nil), nil, nil,
	)
	hooks.OnDelegateFinished("heal__recover", model.DelegateInfo{
		BackendName: "claw", Provider: "anthropic", Tokens: 400, CostUSD: 5,
	})

	h.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-1", CredPlan: plan}, usage, nil, false)
	if buf.Len() != 0 {
		t.Fatalf("the recovery's report logged %q, want silence — its observation joins the runtime-id row, so the fine table rides", buf.String())
	}

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
// empty table is CORRECT here: it rides, the donor books $0, and the warn
// stays silent, or every gateway-served lease run books its whole attempt
// on a donor whose credential never spent and trains its reader to ignore
// the one warn meant to surface real drift. Mutant guards: dropping the
// env-funded exclusion reds (the gateway observation counts as a miss, the
// table is voided, the donor books $4 and the warn fires); voiding every
// EMPTY table reds the ledger read the same way.
func TestRecordPoolSpend_gatewayFundedSpendKeepsTheEmptyTableQuiet(t *testing.T) {
	var buf bytes.Buffer
	h := newPoolHarnessForRun(t, credpoolLimitsForFine(), "run-1")
	h.runner.cfg.Logger = iterlog.New(iterlog.LevelWarn, &buf)

	plan := finePlan(t)
	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New(), plan, nil)
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
	day, _, err := h.ledger.Usage(context.Background(), donorPledgeID(), time.Now().UTC())
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if day.CostUSD != 0 {
		t.Fatalf("the donor's ledger = $%.2f, want $0 — the gateway spend ran on the deployment's env, and the empty table rides", day.CostUSD)
	}

	// The other env-funded shape: a claude_code session that rode the pod's
	// ambient anthropic env (its own label says so). Its credential is the
	// deployment's, never a bundle row — the emptiness is correct too.
	buf.Reset()
	hEnv := newPoolHarnessForRun(t, credpoolLimitsForFine(), "run-4")
	hEnv.runner.cfg.Logger = iterlog.New(iterlog.LevelWarn, &buf)
	usageEnv := newMetricsEmitter(&recordingEmitter{}, metrics.New(), plan, nil)
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
	dayEnv, _, err := hEnv.ledger.Usage(context.Background(), donorPledgeID(), time.Now().UTC())
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if dayEnv.CostUSD != 0 {
		t.Fatalf("the donor's ledger = $%.2f, want $0 — the session spent the pod's ambient env, and the empty table rides", dayEnv.CostUSD)
	}

	// The same shape on a route a plan is supposed to answer is drift: the
	// exclusion is env-funded routes, not the void itself. The impl node's
	// route naming openai joins a key the plan never stamped for it (impl
	// plans the donor's anthropic) — vendor-funded drift, the table voided
	// and the whole attempt booked.
	buf.Reset()
	h2 := newPoolHarnessForRun(t, credpoolLimitsForFine(), "run-2")
	h2.runner.cfg.Logger = iterlog.New(iterlog.LevelWarn, &buf)
	usage2 := newMetricsEmitter(&recordingEmitter{}, metrics.New(), plan, nil)
	hooks2 := model.NewStoreEventHooks(
		context.Background(), usage2, "run-2", iterlog.New(iterlog.LevelError, nil), nil, nil,
	)
	hooks2.OnLLMRequest("impl", model.LLMRequestInfo{Model: "openai/gpt-5", WireModel: "gpt-5"})
	hooks2.OnDelegateFinished("impl", model.DelegateInfo{
		BackendName: "claw", Tokens: 900, CostUSD: 4,
	})
	h2.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-2", TenantID: "team-1", CredPlan: plan}, usage2, nil, false)
	if !strings.Contains(buf.String(), voidedTableWarn) {
		t.Fatalf("a vendor-funded miss logged %q, want the voided-table warn — this one is drift", buf.String())
	}
	day2, _, err := h2.ledger.Usage(context.Background(), donorPledgeID(), time.Now().UTC())
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if day2.CostUSD != 4 {
		t.Fatalf("the donor's ledger = $%.2f, want $4 — a vendor-funded miss voids the table and books the whole attempt", day2.CostUSD)
	}
}

// TestSupervisorUsageSink_JoinsThePlanLikeAnEvent: the PRODUCTION OnUsage
// sink (meterSupervisorSpecs' closure) joins the credential plan the way
// the event paths do — the source IS the node half the walk recorded
// (`supervisor:<name>`), so an evaluation books its row, each token ONCE
// (the observed split, the aggregate left at zero: the donor's counters are
// disjoint and their true total is the sum), and an evaluation the plan
// names nothing for counts as a miss, the one that voids the report's
// table. Mutant guards: the fold dropped from addRoute reds both halves;
// the plan join reading the sink's source as a label reds the first; the
// sink filing the in+out sum beside the split reds the row's counters and
// the miss's token total.
func TestSupervisorUsageSink_JoinsThePlanLikeAnEvent(t *testing.T) {
	sink := func(t *testing.T, plan map[string]string, pin string) (*metricsEmitter, func(in, out int)) {
		t.Helper()
		m := newMetricsEmitter(&recordingEmitter{}, metrics.New(), plan, nil)
		specs := []supervise.Spec{{Name: "pacer", Model: pin}}
		meterSupervisorSpecs(context.Background(), specs, m)
		if specs[0].OnUsage == nil {
			t.Fatal("meterSupervisorSpecs left OnUsage nil — the test premise (the production sink) broke")
		}
		return m, specs[0].OnUsage
	}

	// The plan names the supervisor's site: its evaluation books the
	// donor's slice, the same space the lease stamps.
	plan := map[string]string{"supervisor:pacer|anthropic": donorFingerprint}
	m, evaluate := sink(t, plan, "anthropic/claude-opus-5")
	evaluate(900, 100)
	rows, miss := m.fineSnapshot()
	row, ok := rows[donorFingerprint]
	if !ok {
		t.Fatalf("the sink booked no fine row; table: %v — a supervisor evaluation must join the plan like an event", rows)
	}
	if row.InputTokens != 900 || row.OutputTokens != 100 || row.AggregateTokens != 0 {
		t.Fatalf("the donor's fine row = (in %d, out %d, aggregate %d), want (900, 100, 0) — the evaluation's 1000 tokens, each counted once",
			row.InputTokens, row.OutputTokens, row.AggregateTokens)
	}
	if miss.observations != 0 {
		t.Fatalf("a joined observation counted %d miss(es), want 0", miss.observations)
	}

	// The same sink resolving a provider the plan carries no row for on the
	// supervisor's site counts as a miss — the report voids its table on it,
	// the coarse booking keeps the donor whole. The miss carries the
	// evaluation's own token total, once.
	m2, evaluate2 := sink(t, plan, "openai/gpt-5")
	evaluate2(10, 5)
	if _, miss2 := m2.fineSnapshot(); miss2.observations != 1 || miss2.tokens != 15 {
		t.Fatalf("an unjoined sink observation counted (misses=%d, tokens=%d), want (1, 15)", miss2.observations, miss2.tokens)
	}
}

// TestRecordPoolSpend_aRuntimeNodeTheWalkCannotSeeBooksTheWholeAttempt: a
// claw subagent loop's events carry the `subagent:<agent>` id the walk
// never saw — runtime input the plan cannot cover. Here the subagent of the
// donor-funded impl node spends the donor's own lent subscription (the
// parent's anthropic route), the R30d246 harm through the Task tool: a
// table with impl's row alone would book the donor $2 while their
// credential paid for the subagent too, and any borrower could push spend
// past MaxUSDPerDay through subagents. The miss voids the table: the lease
// books the whole attempt, and one warn names the run. Mutant guards:
// dropping the void (sending the partial table) reds the ledger read ($2);
// dropping the miss accounting reds the same and the warn; dropping the
// warn reds the log assertion.
func TestRecordPoolSpend_aRuntimeNodeTheWalkCannotSeeBooksTheWholeAttempt(t *testing.T) {
	var buf bytes.Buffer
	newLoggedRunner := func(t *testing.T, runID string) *poolHarness {
		t.Helper()
		h := newPoolHarnessForRun(t, credpoolLimitsForFine(), runID)
		h.runner.cfg.Logger = iterlog.New(iterlog.LevelWarn, &buf)
		return h
	}

	h := newLoggedRunner(t, "run-1")
	plan := finePlan(t)
	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New(), plan, nil)
	hooks := model.NewStoreEventHooks(
		context.Background(), usage, "run-1", iterlog.New(iterlog.LevelError, nil), nil, nil,
	)
	// The door fired: the impl node spent the donor's slice ($2).
	hooks.OnDelegateFinished("impl", model.DelegateInfo{
		BackendName: "claude_code", Provider: "anthropic", Tokens: 900, CostUSD: 2,
	})
	// A claw subagent of impl spent the SAME lent subscription — its events
	// name `subagent:auditor`, a node the plan has no row for.
	hooks.OnLLMRequest("subagent:auditor", model.LLMRequestInfo{Model: "anthropic/claude-opus-5", WireModel: "claude-opus-5"})
	hooks.OnLLMStepFinish("subagent:auditor", model.LLMStepInfo{
		Number: 1, InputTokens: 20000, OutputTokens: 2000, Provider: "anthropic",
	})
	whole, _, _, _ := usage.RunTotals()
	if whole <= 2 {
		t.Fatalf("the subagent's step priced nothing (attempt total $%.4f) — the test premise (a priced subagent slice) broke", whole)
	}

	h.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-1", CredPlan: plan}, usage, nil, false)
	if !strings.Contains(buf.String(), voidedTableWarn) || !strings.Contains(buf.String(), "run-1") {
		t.Fatalf("the report logged %q, want the voided-table warn naming the run", buf.String())
	}

	day, _, err := h.ledger.Usage(context.Background(), donorPledgeID(), time.Now().UTC())
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	// The ledger keeps money at milli-cent resolution (CostToMillis).
	if math.Abs(day.CostUSD-whole) >= 1e-3 {
		t.Fatalf("the donor's ledger = $%.6f, want the whole attempt's $%.6f — a slice the plan cannot place voids the fine table, never books the donor short", day.CostUSD, whole)
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

// interactionPlan is the plan for the fired-interaction shape: the impl
// node's `interaction: llm` answers the delegate's questions with a direct
// generation on the donor's lent subscription (its interaction model rides
// anthropic), under the synthetic id `impl_interaction` every event of the
// generation carries (model.ExecuteHumanLLMForInteraction). The walk reads
// the generation at `impl#interaction`; the plan must key a row on the
// runtime id or the fired interaction joins nothing.
func interactionPlan(t *testing.T) map[string]string {
	t.Helper()
	wf := fineBookingWorkflow()
	impl := wf.Nodes["impl"].(*ir.AgentNode)
	impl.InteractionFields = ir.InteractionFields{Interaction: ir.InteractionLLM, InteractionModel: "anthropic/claude-opus-5"}
	res := model.EffectiveProviders(model.RouteView{Workflow: wf}, planKnownProviders)
	if !res.NarrowSafe {
		t.Fatalf("the interaction walk widened — test premise broken: %+v", res)
	}
	plan := model.CredentialPlan(res, fineBookingBundle())
	want := map[string]string{
		"impl|anthropic":             donorFingerprint,
		"impl_interaction|anthropic": donorFingerprint,
		"review|openai":              "fp-own-openai",
	}
	if !maps.Equal(plan, want) {
		t.Fatalf("plan = %v, want %v — the interaction keys under the runtime id, funded by the donor's lent subscription", plan, want)
	}
	return plan
}

// TestRecordPoolSpend_aFiredInteractionBooksTheDonorsSlice: the delegate
// asked, `interaction: llm` answered on the donor's subscription, and every
// event of that generation names `impl_interaction` — the interaction's
// priced steps book the donor's slice through the plan's runtime-id row, the
// table riding with no miss. Mutant guards: the derivation keying the
// interaction on the asking node alone reds at interactionPlan's want-map
// before any booking (in production the steps would miss and void the run's
// fine table — the donor charged the team's own $8 too); the runtime id the
// generation stamps is pinned against the plan in the model package
// (TestExecuteHumanLLMForInteraction_stepsJoinThePlansRuntimeRow).
func TestRecordPoolSpend_aFiredInteractionBooksTheDonorsSlice(t *testing.T) {
	var buf bytes.Buffer
	h := newPoolHarnessForRun(t, credpoolLimitsForFine(), "run-1")
	h.runner.cfg.Logger = iterlog.New(iterlog.LevelWarn, &buf)
	ctx := context.Background()

	plan := interactionPlan(t)
	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New(), plan, nil)
	hooks := model.NewStoreEventHooks(
		context.Background(), usage, "run-1", iterlog.New(iterlog.LevelError, nil), nil, nil,
	)
	hooks.OnDelegateFinished("impl", model.DelegateInfo{
		BackendName: "claude_code", Provider: "anthropic", Tokens: 900, CostUSD: 2,
	})
	hooks.OnLLMRequest("impl_interaction", model.LLMRequestInfo{Model: "anthropic/claude-opus-5", WireModel: "claude-opus-5"})
	hooks.OnLLMStepFinish("impl_interaction", model.LLMStepInfo{
		Number: 1, InputTokens: 20000, OutputTokens: 2000, Provider: "anthropic",
	})
	hooks.OnDelegateFinished("review", model.DelegateInfo{
		BackendName: "codex", Provider: "openai", Tokens: 100, CostUSD: 8,
	})
	whole, _, _, _ := usage.RunTotals()
	slice := whole - 8
	if slice <= 2 {
		t.Fatalf("the interaction's step priced nothing (donor slice $%.4f) — the test premise (a priced interaction) broke", slice)
	}

	h.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-1", CredPlan: plan}, usage, nil, false)
	if buf.Len() != 0 {
		t.Fatalf("the interaction's report logged %q, want silence — its steps join the runtime-id row, so the fine table rides", buf.String())
	}
	day, _, err := h.ledger.Usage(ctx, donorPledgeID(), time.Now().UTC())
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	// The ledger keeps money at milli-cent resolution (CostToMillis).
	if math.Abs(day.CostUSD-slice) >= 1e-3 {
		t.Fatalf("the donor's ledger = $%.6f, want $%.6f — impl's $2 and the interaction that ran on their subscription, never the team's own $8", day.CostUSD, slice)
	}
}

// TestRecordPoolSpend_aFailedDelegationJoinsThePlanLikeAFinishedOne: a
// delegation that dies (a timeout, a rate limit) burned its spend like a
// finished one, and its delegate_error joins the credential plan the same
// way — through the one booking entry, on the event's own provider word.
// One shape per guard, the guard named with the mutant that reds it:
//   - the donor-funded node FAILED: its spend books the donor's slice ($2,
//     never the team's $8) — red when the error path books the route table
//     without the plan join (no donor row: $0);
//   - the team's codex node failed on a bare `gpt-5.5` under its
//     `provider: openai` word: it joins the team's row and the donor books
//     $2 — red when the error path drops the word (the bare model names
//     nothing: a miss, the table voided, the donor charged $10, a warn);
//   - a failed delegation the plan names nothing for voids the table (the
//     whole $5 and a warn) — red when the error path loses its miss.
func TestRecordPoolSpend_aFailedDelegationJoinsThePlanLikeAFinishedOne(t *testing.T) {
	scenarios := []struct {
		name     string
		fire     func(hooks model.EventHooks)
		want     float64
		wantWarn bool
	}{
		{
			name: "the donor's failed delegation books its slice",
			fire: func(hooks model.EventHooks) {
				hooks.OnDelegateError("impl", model.DelegateInfo{
					BackendName: "claude_code", Provider: "anthropic", Tokens: 900, CostUSD: 2,
					Error: errors.New("rate limited mid-run"),
				})
				hooks.OnDelegateFinished("review", model.DelegateInfo{
					BackendName: "codex", Provider: "openai", Tokens: 100, CostUSD: 8,
				})
			},
			want: 2,
		},
		{
			name: "a failure joins on its word, not on its bare model",
			fire: func(hooks model.EventHooks) {
				hooks.OnDelegateFinished("impl", model.DelegateInfo{
					BackendName: "claude_code", Provider: "anthropic", Tokens: 900, CostUSD: 2,
				})
				hooks.OnDelegateError("review", model.DelegateInfo{
					BackendName: "codex", Provider: "openai", RouteModel: "gpt-5.5", Tokens: 100, CostUSD: 8,
					Error: errors.New("timed out"),
				})
			},
			want: 2,
		},
		{
			name: "an unplaced failure voids the table",
			fire: func(hooks model.EventHooks) {
				hooks.OnDelegateFinished("impl", model.DelegateInfo{
					BackendName: "claude_code", Provider: "anthropic", Tokens: 900, CostUSD: 2,
				})
				hooks.OnDelegateError("ghost", model.DelegateInfo{
					BackendName: "claw", Provider: "anthropic", Tokens: 100, CostUSD: 3,
					Error: errors.New("timed out"),
				})
			},
			want:     5,
			wantWarn: true,
		},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			var buf bytes.Buffer
			h := newPoolHarnessForRun(t, credpoolLimitsForFine(), "run-1")
			h.runner.cfg.Logger = iterlog.New(iterlog.LevelWarn, &buf)
			plan := finePlan(t)
			usage := newMetricsEmitter(&recordingEmitter{}, metrics.New(), plan, nil)
			sc.fire(model.NewStoreEventHooks(
				context.Background(), usage, "run-1", iterlog.New(iterlog.LevelError, nil), nil, nil,
			))

			h.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-1", CredPlan: plan}, usage, nil, false)
			if warned := strings.Contains(buf.String(), voidedTableWarn); warned != sc.wantWarn {
				t.Fatalf("the report logged %q, want the voided-table warn: %v", buf.String(), sc.wantWarn)
			}
			day, _, err := h.ledger.Usage(context.Background(), donorPledgeID(), time.Now().UTC())
			if err != nil {
				t.Fatalf("usage: %v", err)
			}
			if day.CostUSD != sc.want {
				t.Fatalf("the donor's ledger = $%.2f, want $%.2f", day.CostUSD, sc.want)
			}
		})
	}
}

// TestMetricsEmitter_aStepJoinsOnItsLoopsWordOverTheRecordedRoute: the step
// branch reads the loop's own provider word FIRST — the slot its client was
// resolved from — as every delegate branch reads the event's word; the
// node's recorded route is the runner's re-derivation, which can name
// another provider the plan holds a row for. The divergence is constructed
// (today's claw loops stamp the word their request's spec also derives), so
// this is the shape that reds a step join that stops reading the word: the
// recorded openai route would take the donor's step onto the team's row,
// booking the donor short.
func TestMetricsEmitter_aStepJoinsOnItsLoopsWordOverTheRecordedRoute(t *testing.T) {
	wf := fineBookingWorkflow()
	impl := wf.Nodes["impl"].(*ir.AgentNode)
	impl.Fallbacks = []ir.Fallback{{Name: "own-rescue", Provider: "openai", Model: "openai/gpt-5"}}
	res := model.EffectiveProviders(model.RouteView{Workflow: wf}, planKnownProviders)
	if !res.NarrowSafe {
		t.Fatalf("the two-route walk widened — test premise broken: %+v", res)
	}
	plan := model.CredentialPlan(res, fineBookingBundle())
	if plan["impl|anthropic"] != donorFingerprint || plan["impl|openai"] != "fp-own-openai" {
		t.Fatalf("plan = %v, want impl rows for both its routes — the premise (two planned providers on one node) broke", plan)
	}

	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New(), plan, nil)
	hooks := model.NewStoreEventHooks(
		context.Background(), usage, "run-1", iterlog.New(iterlog.LevelError, nil), nil, nil,
	)
	hooks.OnLLMRequest("impl", model.LLMRequestInfo{Model: "openai/gpt-5", WireModel: "gpt-5"})
	hooks.OnLLMStepFinish("impl", model.LLMStepInfo{
		Number: 1, InputTokens: 20000, OutputTokens: 2000, Provider: "anthropic",
	})

	rows, miss := usage.fineSnapshot()
	if miss.observations != 0 {
		t.Fatalf("a step its word places counted %d miss(es), want 0", miss.observations)
	}
	donor := rows[donorFingerprint]
	if donor.InputTokens != 20000 || donor.OutputTokens != 2000 {
		t.Fatalf("the donor's fine row = (in %d, out %d), want (20000, 2000) — the step books on its loop's word; table: %v", donor.InputTokens, donor.OutputTokens, rows)
	}
	if own, booked := rows["fp-own-openai"]; booked {
		t.Fatalf("the team's row took the step (%+v) — the recorded route answered over the loop's word", own)
	}
}

// TestRecordPoolSpend_onlyAnUnplacedObservationThatSpentVoidsTheTable: the
// void answers spend the plan cannot place, so an unplaced observation that
// spent nothing — no cost, no tokens: a claw sub-agent step whose provider
// reported no usage — leaves the table riding, the donor booked their $2
// slice and no warn. Voiding on it would charge them the team's own $5 too,
// for a call that adds nothing to either booking. Each half of a spend still
// voids on its own: a priced call that reported no tokens, and tokens no
// price source knows — the whole attempt and the warn. One scenario per half
// of the spendless exclusion, each isolated. Mutant guards: dropping the
// exclusion reds the free step ($7 and the warn); narrowing it to its token
// half (`tokens == 0`) reds the priced call ($2, no warn); narrowing it to
// its cost half (`cost == 0`) reds the unpriced tokens ($2, no warn).
func TestRecordPoolSpend_onlyAnUnplacedObservationThatSpentVoidsTheTable(t *testing.T) {
	scenarios := []struct {
		name     string
		fire     func(hooks model.EventHooks)
		want     float64
		wantWarn bool
		// reached proves the scenario's observation got to the booking: a
		// riding table passes just as well on an event the emitter dropped.
		// Nil where the warn the scenario wants proves it.
		reached func(t *testing.T, usage *metricsEmitter)
	}{
		{
			name: "a free sub-agent step leaves the table riding",
			fire: func(hooks model.EventHooks) {
				hooks.OnLLMRequest("subagent:auditor", model.LLMRequestInfo{Model: "anthropic/claude-opus-5", WireModel: "claude-opus-5"})
				hooks.OnLLMStepFinish("subagent:auditor", model.LLMStepInfo{
					Number: 1, Provider: "anthropic", UsageUnreported: true,
				})
			},
			want: 2,
			reached: func(t *testing.T, usage *metricsEmitter) {
				t.Helper()
				k := routeKey{backend: delegate.BackendClaw, model: "anthropic/claude-opus-5"}
				if got := usage.RouteTotals()[k].unreportedCalls; got != 1 {
					t.Fatalf("the sub-agent's route counted %d unreported call(s), want 1 — the free step never reached the booking, the test premise broke", got)
				}
			},
		},
		{
			name: "a priced call without tokens voids the table",
			fire: func(hooks model.EventHooks) {
				hooks.OnDelegateFinished("ghost", model.DelegateInfo{
					BackendName: "claude_code", Provider: "anthropic", CostUSD: 1,
				})
			},
			want:     8,
			wantWarn: true,
		},
		{
			name: "tokens no price source knows void the table",
			fire: func(hooks model.EventHooks) {
				hooks.OnDelegateFinished("ghost", model.DelegateInfo{
					BackendName: "claude_code", Provider: "anthropic", Tokens: 100,
				})
			},
			want:     7,
			wantWarn: true,
		},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			var buf bytes.Buffer
			h := newPoolHarnessForRun(t, credpoolLimitsForFine(), "run-1")
			h.runner.cfg.Logger = iterlog.New(iterlog.LevelWarn, &buf)
			plan := finePlan(t)
			usage := newMetricsEmitter(&recordingEmitter{}, metrics.New(), plan, nil)
			hooks := model.NewStoreEventHooks(
				context.Background(), usage, "run-1", iterlog.New(iterlog.LevelError, nil), nil, nil,
			)
			// The door fired on the donor's subscription ($2) and the team's
			// own key spent $5: the table places both.
			hooks.OnDelegateFinished("impl", model.DelegateInfo{
				BackendName: "claude_code", Provider: "anthropic", Tokens: 900, CostUSD: 2,
			})
			hooks.OnDelegateFinished("review", model.DelegateInfo{
				BackendName: "codex", Provider: "openai", Tokens: 100, CostUSD: 5,
			})
			sc.fire(hooks)
			if sc.reached != nil {
				sc.reached(t, usage)
			}

			h.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-1", CredPlan: plan}, usage, nil, false)
			warned := strings.Contains(buf.String(), voidedTableWarn)
			day, _, err := h.ledger.Usage(context.Background(), donorPledgeID(), time.Now().UTC())
			if err != nil {
				t.Fatalf("usage: %v", err)
			}
			if warned != sc.wantWarn || day.CostUSD != sc.want {
				t.Fatalf("the donor's ledger = $%.2f with the voided-table warn %v, want $%.2f with %v; the report logged %q",
					day.CostUSD, warned, sc.want, sc.wantWarn, buf.String())
			}
		})
	}
}

// TestMetricsEmitter_aPlanlessEmitterCountsNoMiss: fineSnapshot answers both
// halves empty on a publication that carried no plan — every observation of
// a plan-less run joins nothing, and none is a miss: there is no plan to
// miss. The report never reads a plan-less emitter's miss
// (fineFingerprintSpend keeps the coarse booking first), so this is the one
// witness of notePlanMissLocked's plan-less return. Mutant guard: dropping
// that return counts the attempt's two priced delegations as misses.
func TestMetricsEmitter_aPlanlessEmitterCountsNoMiss(t *testing.T) {
	rows, miss := fineBookingUsage(t, nil).fineSnapshot()
	if len(rows) != 0 || miss != (planMiss{}) {
		t.Fatalf("a plan-less emitter holds fine rows %v and miss %+v, want neither — a run with no plan has no plan to miss", rows, miss)
	}
}
