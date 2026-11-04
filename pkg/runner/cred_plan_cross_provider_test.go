package runner

import (
	"bytes"
	"context"
	"maps"
	"math"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
)

// A fallback chain's folded spend across two providers, joined END TO END
// (#2288): the producer's plan, the REAL executor's chain walk through the
// production hooks, the meter's plan join and the broker's booking. The
// chain books its failed routes' spend on the ONE delegate event of the
// route that ends it, and the plan keys one credential per (node,
// provider): the event itemises each abandoned route's slice, and the meter
// books each on its own row — a table that placed the fold whole on the
// serving word's row would book the donor short.

// crossProviderNode is the door-served shape: the node's own route spends
// the donor's lent Claude subscription (anthropic), its `gpt` rung the
// team's own openai key through a hint-less codex.
func crossProviderNode() *ir.AgentNode {
	return agentNode(delegate.BackendClaudeCode, "anthropic/claude-opus-5",
		ir.Fallback{Name: "gpt", Backend: delegate.BackendCodex, Model: "openai/gpt-5", On: []string{"transient_exhausted"}})
}

// crossProviderPlan runs the PRODUCER over crossProviderNode and the sealed
// bundle of the fine-booking tests: the node's anthropic row names the
// donor, its openai row the team's own key.
func crossProviderPlan(t *testing.T) map[string]string {
	t.Helper()
	wf := &ir.Workflow{Nodes: map[string]ir.Node{"impl": crossProviderNode()}}
	res := model.EffectiveProviders(model.RouteView{Workflow: wf}, planKnownProviders)
	if !res.NarrowSafe {
		t.Fatalf("the cross-provider walk widened — test premise broken: %+v", res)
	}
	plan := model.CredentialPlan(res, fineBookingBundle())
	want := map[string]string{
		"impl|anthropic": donorFingerprint,
		"impl|openai":    "fp-own-openai",
	}
	if !maps.Equal(plan, want) {
		t.Fatalf("the producer's plan = %v, want %v", plan, want)
	}
	return plan
}

// runCrossProviderChain walks crossProviderNode through the real executor:
// the primary burns primaryUSD per attempt on the donor's subscription and
// fails transiently twice, the codex rung spends $0.10 per call — serving,
// or failing with rungErr.
func runCrossProviderChain(t *testing.T, plan map[string]string, primaryUSD float64, rungErr error) *metricsEmitter {
	t.Helper()
	primary := delegate.Result{BackendName: delegate.BackendClaudeCode, SessionFingerprint: "anthropic-oauth"}
	if primaryUSD > 0 {
		primary.Tokens, primary.CostUSD = 500, primaryUSD
		primary.Output = map[string]any{"_tokens": 500, "_cost_usd": primaryUSD}
	}
	backends := map[string]*scriptedBackend{
		delegate.BackendClaudeCode: {res: primary, err: &delegate.ErrTransient{Reason: "overloaded"}},
		delegate.BackendCodex:      {res: delegate.Result{BackendName: delegate.BackendCodex, Tokens: 10, CostUSD: 0.10, Output: map[string]any{"_tokens": 10, "_cost_usd": 0.10}}, err: rungErr},
	}
	regd := map[string]delegate.Backend{}
	for name, b := range backends {
		regd[name] = b
	}
	usage, _ := runChainPlanned(t, crossProviderNode(), regd, backends,
		model.RetryPolicy{MaxAttempts: 2, MaxAttemptsTransient: 2, BackoffBase: time.Millisecond}, plan)
	return usage
}

// TestRecordPoolSpend_aFoldAcrossProvidersBooksEachSliceOnItsRow: the
// review's probe, mirrored onto the seeded donor. The primary burns $5
// twice on the donor's subscription and fails; the codex rung serves for
// $0.10 on the team's own key. The one delegate_finished carries $10.10
// under "openai": joined whole, the fine table books it on the team's key
// and the donor $0 for $10 that ran on their credential. The event itemises
// the primary's slice instead, the meter books it on the donor's row and
// the rung's own $0.10 on the team's, and the table rides.
//
// The exhausted twin: the rung fails too, and the delegate_error that books
// the chain's whole spend ($5 ×2 + $0.10 ×2) under the rung's labels splits
// the same way.
//
// Control: the same chain whose primary spent nothing folds nothing, so the
// table books the donor's empty slice ($0, no warn).
func TestRecordPoolSpend_aFoldAcrossProvidersBooksEachSliceOnItsRow(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		rungErr error
		rung    float64
	}{
		{"the primary spent on the donor, the rung served", nil, 0.10},
		{"the primary spent on the donor, the rung failed too", &delegate.ErrTransient{Reason: "overloaded"}, 0.20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			h := newPoolHarnessForRun(t, credpoolLimitsForFine(), "run-1")
			h.runner.cfg.Logger = iterlog.New(iterlog.LevelWarn, &buf)
			plan := crossProviderPlan(t)
			usage := runCrossProviderChain(t, plan, 5, tc.rungErr)

			costUSD, _, _, _ := usage.RunTotals()
			if math.Abs(costUSD-(10+tc.rung)) > 1e-9 {
				t.Fatalf("the run totals = $%.6f, want the chain's folded $%.2f — test premise broken", costUSD, 10+tc.rung)
			}
			rows, miss := usage.fineSnapshot()
			if miss.observations != 0 {
				t.Fatalf("the plan missed %+v — every slice of the fold has its own row", miss)
			}
			if math.Abs(rows[donorFingerprint].CostUSD-10) > 1e-9 || math.Abs(rows["fp-own-openai"].CostUSD-tc.rung) > 1e-9 {
				t.Fatalf("the fine rows = %+v, want the donor's $10 and the team's $%.2f", rows, tc.rung)
			}
			h.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-1", CredPlan: plan}, usage, nil, false)

			day, _, err := h.ledger.Usage(ctx, donorPledgeID(), time.Now().UTC())
			if err != nil {
				t.Fatalf("usage: %v", err)
			}
			if math.Abs(day.CostUSD-10) >= 1e-3 {
				t.Fatalf("the donor's ledger = $%.4f, want the $10 that ran on their subscription — the rung's spend ran on the team's key", day.CostUSD)
			}
			if buf.Len() != 0 {
				t.Fatalf("a table every slice joined logged %q, want silence", buf.String())
			}
		})
	}

	t.Run("the primary spent nothing", func(t *testing.T) {
		var buf bytes.Buffer
		h := newPoolHarnessForRun(t, credpoolLimitsForFine(), "run-2")
		h.runner.cfg.Logger = iterlog.New(iterlog.LevelWarn, &buf)
		plan := crossProviderPlan(t)
		usage := runCrossProviderChain(t, plan, 0, nil)
		h.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-2", TenantID: "team-1", CredPlan: plan}, usage, nil, false)

		day, _, err := h.ledger.Usage(ctx, donorPledgeID(), time.Now().UTC())
		if err != nil {
			t.Fatalf("usage: %v", err)
		}
		if day.CostUSD != 0 {
			t.Fatalf("the donor's ledger = $%.4f, want $0 — only the team's own key spent, and the table places it", day.CostUSD)
		}
		if buf.Len() != 0 {
			t.Fatalf("a table every observation joined logged %q, want silence", buf.String())
		}
	})
}

// TestRecordPoolSpend_aFoldEndingOnTheGatewayBooksTheDonorsSlice: an
// env-funded route places nobody and voids nothing by design — and a fold
// that ENDS on one names only where the chain finished. The donor's $10
// burned on the primary rides inside the gateway rung's figure, itemised:
// it books on the donor's row, the gateway's own $0.50 on nobody, and the
// table rides. Booked whole on the gateway's labels, the donor's slice
// would be placed on nobody.
func TestRecordPoolSpend_aFoldEndingOnTheGatewayBooksTheDonorsSlice(t *testing.T) {
	var buf bytes.Buffer
	h := newPoolHarnessForRun(t, credpoolLimitsForFine(), "run-1")
	h.runner.cfg.Logger = iterlog.New(iterlog.LevelWarn, &buf)
	plan := finePlan(t)
	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New(), plan)
	hooks := model.NewStoreEventHooks(
		context.Background(), usage, "run-1", iterlog.New(iterlog.LevelError, nil), nil, nil,
	)
	hooks.OnDelegateFinished("impl", model.DelegateInfo{
		BackendName: delegate.BackendClaw, RouteModel: "openai_compatible/team-a/m",
		Tokens: 1050, CostUSD: 10.5,
		FoldedSpend: []model.SpendSlice{{
			BackendName: delegate.BackendClaudeCode, RouteModel: "anthropic/claude-opus-5",
			Fingerprint: "anthropic-oauth", Tokens: 1000, CostUSD: 10,
		}},
	})
	h.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-1", CredPlan: plan}, usage, nil, false)

	day, _, err := h.ledger.Usage(context.Background(), donorPledgeID(), time.Now().UTC())
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if math.Abs(day.CostUSD-10) >= 1e-3 {
		t.Fatalf("the donor's ledger = $%.4f, want their $10 — the gateway route the fold ended on does not fund the slice it carries", day.CostUSD)
	}
	if buf.Len() != 0 {
		t.Fatalf("the report logged %q, want silence — the donor's slice joins their row, the gateway's is env-funded", buf.String())
	}
}

// TestPlanProviderFor_codexNamesOpenAI: a codex route names the one
// provider the CLI serves, its model's prefix or not — a bare `gpt-5` names
// none to derive from.
func TestPlanProviderFor_codexNamesOpenAI(t *testing.T) {
	for _, mdl := range []string{"gpt-5", "openai/gpt-5", ""} {
		if got := planProviderFor(routeKey{backend: delegate.BackendCodex, model: mdl}); got != "openai" {
			t.Errorf("planProviderFor(codex, %q) = %q, want openai", mdl, got)
		}
	}
}

// TestRecordPoolSpend_aWordlessCodexEventJoinsTheOpenAIRow: a codex delegate
// event that carries no provider word and a bare model joins the plan's
// openai row — the runner's derivation answers what the executor stamps —
// instead of missing and voiding a table that places everything else.
func TestRecordPoolSpend_aWordlessCodexEventJoinsTheOpenAIRow(t *testing.T) {
	var buf bytes.Buffer
	h := newPoolHarnessForRun(t, credpoolLimitsForFine(), "run-1")
	h.runner.cfg.Logger = iterlog.New(iterlog.LevelWarn, &buf)
	plan := finePlan(t)
	usage := newMetricsEmitter(&recordingEmitter{}, metrics.New(), plan)
	hooks := model.NewStoreEventHooks(
		context.Background(), usage, "run-1", iterlog.New(iterlog.LevelError, nil), nil, nil,
	)
	hooks.OnDelegateFinished("impl", model.DelegateInfo{
		BackendName: "claude_code", Provider: "anthropic", Tokens: 900, CostUSD: 2,
	})
	hooks.OnDelegateFinished("review", model.DelegateInfo{
		BackendName: delegate.BackendCodex, RouteModel: "gpt-5", Tokens: 100, CostUSD: 8,
	})
	h.runner.recordPoolSpend(&queue.RunMessage{RunID: "run-1", TenantID: "team-1", CredPlan: plan}, usage, nil, false)

	day, _, err := h.ledger.Usage(context.Background(), donorPledgeID(), time.Now().UTC())
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if day.CostUSD != 2 {
		t.Fatalf("the donor's ledger = $%.2f, want $2 — the codex review's $8 ran on the team's own openai key", day.CostUSD)
	}
	if buf.Len() != 0 {
		t.Fatalf("the report logged %q, want silence — the codex event joins the openai row", buf.String())
	}
}
