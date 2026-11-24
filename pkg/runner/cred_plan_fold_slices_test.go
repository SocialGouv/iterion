package runner

import (
	"bytes"
	"context"
	"maps"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
)

// A fallback chain books its abandoned routes' spend on the ONE delegate
// event of the route that ends it (#2288). The event itemises those routes'
// slices, and the meter books each on its own route and its own plan row —
// a claw slice whose loop's steps it already priced is skipped, any other
// slice is booked — whatever the backend of the route that ended the chain.
// These witnesses drive the REAL executor through the production hooks into
// the meter and the broker, with the plan the producer stamps.

// foldPlan runs the PRODUCER over nodes and the fine-booking bundle, and
// fails when it does not answer want — the premise every witness below
// books against.
func foldPlan(t *testing.T, nodes map[string]ir.Node, want map[string]string) map[string]string {
	t.Helper()
	res := model.EffectiveProviders(model.RouteView{Workflow: &ir.Workflow{Nodes: nodes}}, planKnownProviders)
	if !res.NarrowSafe {
		t.Fatalf("the walk widened — test premise broken: %+v", res)
	}
	plan := model.CredentialPlan(res, fineBookingBundle())
	if !maps.Equal(plan, want) {
		t.Fatalf("the producer's plan = %v, want %v — test premise broken", plan, want)
	}
	return plan
}

// clawStepPrice is what the meter prices one claw step of in/out tokens on
// modelSpec at — the figure the steps book, independent of the claw
// result's own CostUSD.
func clawStepPrice(t *testing.T, modelSpec string, in, out int) float64 {
	t.Helper()
	const probe = 1_000_000
	price := float64(in)*cost.EstimateUSD(modelSpec, probe, 0)/probe + float64(out)*cost.EstimateUSD(modelSpec, 0, probe)/probe
	if price <= 0 {
		t.Fatalf("%s prices to $%v — test premise (priced claw steps) broken", modelSpec, price)
	}
	return price
}

// bookAndReadDonor reports the attempt with plan and returns the donor's
// ledger figure and the report's log.
func bookAndReadDonor(t *testing.T, usage *metricsEmitter, plan map[string]string) (float64, string) {
	t.Helper()
	var buf bytes.Buffer
	h := newPoolHarnessForRun(t, credpoolLimitsForFine(), "run-1")
	h.runner.cfg.Logger = iterlog.New(iterlog.LevelWarn, &buf)
	h.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-1", CredPlan: plan}, usage, nil, false)
	day, _, err := h.ledger.Usage(context.Background(), donorPledgeID(), time.Now().UTC())
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	return day.CostUSD, buf.String()
}

// TestFoldedSpend_aSliceUnderAStepObservedClawRungIsBooked: the ticket's
// probe on the claw door as it runs in production — its loop fires
// llm_step_finished through the hooks. The donor's subscription burns $5
// twice on the claude_code primary and fails; the claw rung on the team's
// own openai key serves with one priced step. The claw rung's
// delegate_finished carries the folded $10 and, its steps already priced,
// the summarised guard skips the claw figure — but the folded slice never
// was a step: it books on the donor's row, the claw slice on the team's,
// the table rides, and the run totals keep the $10.
func TestFoldedSpend_aSliceUnderAStepObservedClawRungIsBooked(t *testing.T) {
	node := agentNode(delegate.BackendClaudeCode, "anthropic/claude-opus-5",
		ir.Fallback{Name: "api", Backend: delegate.BackendClaw, Model: "openai/gpt-5", On: []string{"transient_exhausted"}})
	plan := foldPlan(t, map[string]ir.Node{"impl": node}, map[string]string{
		"impl|anthropic": donorFingerprint,
		"impl|openai":    "fp-own-openai",
	})
	steps := clawStepPrice(t, "openai/gpt-5", 1000, 100)
	backends := map[string]*scriptedBackend{
		delegate.BackendClaudeCode: {
			res: delegate.Result{BackendName: delegate.BackendClaudeCode, SessionFingerprint: "anthropic-oauth", Tokens: 500, CostUSD: 5,
				Output: map[string]any{"_tokens": 500, "_cost_usd": 5.0}},
			err: &delegate.ErrTransient{Reason: "overloaded"},
		},
		delegate.BackendClaw: {
			res:   delegate.Result{BackendName: delegate.BackendClaw, Tokens: 1100, CostUSD: 0.10, Output: map[string]any{"_tokens": 1100, "_cost_usd": 0.10}},
			steps: [][2]int{{1000, 100}},
		},
	}
	regd := map[string]delegate.Backend{}
	for name, b := range backends {
		regd[name] = b
	}
	usage, _ := runChainPlanned(t, node, regd, backends,
		model.RetryPolicy{MaxAttempts: 2, MaxAttemptsTransient: 2, BackoffBase: time.Millisecond}, plan)

	if costUSD, _, _, _ := usage.RunTotals(); math.Abs(costUSD-(10+steps)) > 1e-9 {
		t.Fatalf("the run totals = $%.6f, want the primary's $10 plus the claw step's $%.6f — the folded slice was dropped with the summarised claw figure", costUSD, steps)
	}
	donor, logged := bookAndReadDonor(t, usage, plan)
	if math.Abs(donor-10) >= 1e-3 {
		t.Fatalf("the donor's ledger = $%.4f, want the $10 that ran on their subscription", donor)
	}
	if strings.Contains(logged, voidedTableWarn) {
		t.Fatalf("the report voided its table (%q) — every slice has its own row", logged)
	}
}

// TestFoldedSpend_aStepObservedClawSliceUnderAnotherRungIsBookedOnce: the
// mirror. The claw primary on the team's own openai key prices one step,
// then fails; the claude_code rung on the donor's subscription serves. Its
// delegate_finished folds the claw result's figure, which the steps
// already booked: the claw slice is skipped, the donor's slice books on
// their row, and the table rides.
func TestFoldedSpend_aStepObservedClawSliceUnderAnotherRungIsBookedOnce(t *testing.T) {
	node := agentNode(delegate.BackendClaw, "openai/gpt-5",
		ir.Fallback{Name: "door", Backend: delegate.BackendClaudeCode, Model: "anthropic/claude-opus-5", On: []string{"transient_exhausted"}})
	plan := foldPlan(t, map[string]ir.Node{"impl": node}, map[string]string{
		"impl|anthropic": donorFingerprint,
		"impl|openai":    "fp-own-openai",
	})
	steps := clawStepPrice(t, "openai/gpt-5", 1000, 100)
	backends := map[string]*scriptedBackend{
		delegate.BackendClaw: {
			res:   delegate.Result{BackendName: delegate.BackendClaw, Tokens: 1100, CostUSD: 3, Output: map[string]any{"_tokens": 1100, "_cost_usd": 3.0}},
			err:   &delegate.ErrTransient{Reason: "overloaded"},
			steps: [][2]int{{1000, 100}},
		},
		delegate.BackendClaudeCode: {
			res: delegate.Result{BackendName: delegate.BackendClaudeCode, SessionFingerprint: "anthropic-oauth", Tokens: 200, CostUSD: 2,
				Output: map[string]any{"_tokens": 200, "_cost_usd": 2.0}},
		},
	}
	regd := map[string]delegate.Backend{}
	for name, b := range backends {
		regd[name] = b
	}
	usage, _ := runChainPlanned(t, node, regd, backends,
		model.RetryPolicy{MaxAttempts: 1, MaxAttemptsTransient: 1, BackoffBase: time.Millisecond}, plan)

	if costUSD, _, _, _ := usage.RunTotals(); math.Abs(costUSD-(2+steps)) > 1e-9 {
		t.Fatalf("the run totals = $%.6f, want the claw step's $%.6f plus the rung's $2 — the folded claw figure was booked on top of its steps", costUSD, steps)
	}
	donor, logged := bookAndReadDonor(t, usage, plan)
	if math.Abs(donor-2) >= 1e-3 {
		t.Fatalf("the donor's ledger = $%.4f, want the $2 the rung spent on their subscription", donor)
	}
	if strings.Contains(logged, voidedTableWarn) {
		t.Fatalf("the report voided its table (%q) — every slice has its own row", logged)
	}
}

// TestFoldedSpend_aSameProviderFallThroughKeepsTheTable: a fold whose
// slices all join ONE plan row is no miss. The donor's subscription serves
// the impl node through a hint-less claude_code primary that spends $5 and
// hits its window, then a rung on the same provider that serves for $0.10
// — a claw api rung, or a claude_code sonnet rung on the same subscription;
// the review node spends $8 on the team's own openai key. Both impl slices
// join impl|anthropic through the meter's own derivation, so the table
// rides and the donor books $5.10 — never the team's $8.
func TestFoldedSpend_aSameProviderFallThroughKeepsTheTable(t *testing.T) {
	window := func(usd float64) delegate.Result {
		return delegate.Result{BackendName: delegate.BackendClaudeCode, SessionFingerprint: "anthropic-oauth", Tokens: 500, CostUSD: usd,
			Output: map[string]any{"_tokens": 500, "_cost_usd": usd}}
	}
	served := func(backend, fingerprint string) delegate.Result {
		return delegate.Result{BackendName: backend, SessionFingerprint: fingerprint, Tokens: 10, CostUSD: 0.10,
			Output: map[string]any{"_tokens": 10, "_cost_usd": 0.10}}
	}
	for _, tc := range []struct {
		name string
		rung ir.Fallback
		regd func() map[string]delegate.Backend
	}{
		{
			name: "a claw api rung",
			rung: ir.Fallback{Name: "api", Backend: delegate.BackendClaw, Model: "anthropic/claude-opus-5", On: []string{"usage_window"}},
			regd: func() map[string]delegate.Backend {
				return map[string]delegate.Backend{
					delegate.BackendClaudeCode: &scriptedBackend{res: window(5), err: usageWindowErr()},
					delegate.BackendClaw:       &scriptedBackend{res: served(delegate.BackendClaw, "")},
				}
			},
		},
		{
			name: "a claude_code sonnet rung on the same subscription",
			rung: ir.Fallback{Name: "sonnet", Model: "anthropic/claude-sonnet-5", On: []string{"usage_window"}},
			regd: func() map[string]delegate.Backend {
				return map[string]delegate.Backend{delegate.BackendClaudeCode: &attemptsBackend{steps: []struct {
					res delegate.Result
					err error
				}{
					{res: window(5), err: usageWindowErr()},
					{res: served(delegate.BackendClaudeCode, "anthropic-oauth")},
				}}}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := agentNode(delegate.BackendClaudeCode, "anthropic/claude-opus-5", tc.rung)
			plan := foldPlan(t, map[string]ir.Node{
				"impl":   node,
				"review": agentNodeForPlan("review", "openai", "openai/gpt-5"),
			}, map[string]string{
				"impl|anthropic": donorFingerprint,
				"review|openai":  "fp-own-openai",
			})
			usage, _ := runChainPlanned(t, node, tc.regd(), nil, model.RetryPolicy{MaxAttempts: 1, MaxAttemptsTransient: 1}, plan)
			hooks := model.NewStoreEventHooks(context.Background(), usage, "run-1", iterlog.Nop(), nil, nil)
			hooks.OnDelegateFinished("review", model.DelegateInfo{
				BackendName: delegate.BackendCodex, Provider: "openai", RouteModel: "openai/gpt-5", Tokens: 100, CostUSD: 8,
			})

			donor, logged := bookAndReadDonor(t, usage, plan)
			if strings.Contains(logged, voidedTableWarn) {
				t.Fatalf("the report voided its table (%q) — both impl slices join impl|anthropic", logged)
			}
			if math.Abs(donor-5.10) >= 1e-3 {
				t.Fatalf("the donor's ledger = $%.4f, want impl's $5.10 — the review's $8 ran on the team's own key", donor)
			}
		})
	}
}
