package runner

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/internal/credparity"
	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/supervise"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// TestCredParity_MeterLedgerAndPlan drives every row of ADR-161's parity table
// (internal/credparity) through the runner's three readers of a call's spend
// and judges each against the row:
//
//   - the meter: the usage-cap pre-flight's key — a node's route as the
//     pre-flight enumerates it (model.AnthropicWireRoutes over the node's
//     workflow, under the run's allowlist), keyed by runCredKeys.routeKey on
//     the run's sandbox; a supervisor as the pre-flight walks it
//     (preflightSupervisors, the spawn's own list, keyed by supervisorKey);
//   - the ledger: the run slot the spend ledger books the call's spend on
//     (ledgerSlot) — for a claw node, off the step event its dispatch writes:
//     the arm's Spend for the spent leg as its credential keys, the event
//     the metrics emitter observes; for every other call, off the route its
//     events name;
//   - the plan: the fingerprint the fine table books that step on, through
//     the plan the publisher derives (model.CredentialPlan over
//     model.EffectiveProviders) — on the rows whose run carries a plan.
//
// A row nothing spends on books nothing: the ledger and the plan read no
// observation there. The run credentials lookups are the runner's
// (cmd/iterion installs them at boot): the claw arm reads the run's keys
// through them.
func TestCredParity_MeterLedgerAndPlan(t *testing.T) {
	t.Cleanup(model.SetCredentialsLookup(model.RunCredentialsLookup))
	t.Cleanup(model.SetOAuthDirLookup(model.RunOAuthDirLookup))
	for _, row := range credparity.Rows() {
		t.Run(row.Label(), func(t *testing.T) {
			forfaitDir := applyParityPosture(t, row)
			creds := row.Run.Credentials(forfaitDir)
			ctx := secrets.WithCredentials(context.Background(), creds)
			msg := &queue.RunMessage{RunID: "run-parity", TenantID: "team-a", LLMProviders: row.Run.Allowlist}
			// The row's call site, of its kind (model.CallSites' vocabulary).
			node := row.Node.IR()
			wf := &ir.Workflow{Name: "parity", Nodes: map[string]ir.Node{row.Node.ID: node}}

			keys := usageCapCredKeys(ctx, msg)
			keys.sandboxed = row.Sandboxed
			meterOf := func(s credparity.Spend) string {
				switch {
				case s.Refused():
					return "not metered"
				case s.Channel == credparity.OwnConfig:
					// Keyed as before the resolver (ADR-161 decision 5):
					// opencode on the run's unslotted meter, kimi and grok
					// not metered.
					if row.Node.Backend == delegate.BackendOpenCode {
						return keys.keyForSlot("")
					}
					return "not metered"
				case s.Slot != "" && secrets.WireFamily(s.Slot) != secrets.WireFamilyAnthropic:
					// The cap meters the anthropic wire, the wire its
					// readings come from.
					return "not metered"
				case s.Slot != "":
					return keys.keyForSlot(s.Slot)
				case s.Channel == credparity.PodEnv:
					return usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, secrets.FingerprintSHA256(s.Credential))
				}
				return keys.keyForSlot("")
			}
			want := row.Want
			if row.Role == credparity.SupervisorRole {
				credparity.Check(t, row, credparity.Meter, supervisorParityMeter(t, ctx, row, msg, keys), meterOf)
				// The supervisor's evaluations book through the OnUsage sink,
				// whose observation carries no credential keys: the ledger
				// reads its route.
				if !want.Refused() {
					route := routeKey{backend: delegate.BackendClaw, model: row.Node.Spec(want.Leg), source: model.SupervisorSiteID(parityRowSupervisor, 0)}
					slot, _ := ledgerSlot(creds, route)
					credparity.Check(t, row, credparity.Ledger, slot, func(s credparity.Spend) string { return s.Slot })
				}
				return
			}

			got := "not metered"
			view := model.RouteView{Workflow: wf, AllowedProviders: row.Run.Allowlist}
			for _, route := range model.AnthropicWireRoutes(view) {
				if route.NodeID != row.Node.SiteID() {
					continue
				}
				if key, metered := keys.routeKey(ctx, route); metered {
					got = key
				}
			}
			credparity.Check(t, row, credparity.Meter, got, meterOf)

			if want.Refused() {
				return
			}
			var plan map[string]string
			if row.Run.Plan {
				plan = model.CredentialPlan(model.EffectiveProviders(model.RouteView{Workflow: wf}, model.KnownProviderSet()), row.Run.Bundle())
				if len(plan) == 0 {
					t.Fatalf("inert bench: the publisher stamps no plan for this run")
				}
			}
			em := newMetricsEmitter(nil, nil, plan, nil)
			route := parityObservation(t, ctx, row, view, em)
			slot, _ := ledgerSlot(creds, route)
			credparity.Check(t, row, credparity.Ledger, slot, func(s credparity.Spend) string { return s.Slot })

			if !row.Run.Plan {
				return
			}
			rows, miss := em.fineSnapshot()
			if miss.observations > 0 {
				t.Fatalf("the plan answered nothing for the row's step: %+v", miss)
			}
			booked := ""
			for fp := range rows {
				booked = fp
			}
			if len(rows) > 1 {
				t.Fatalf("one step booked %d fine rows: %v", len(rows), rows)
			}
			credparity.Check(t, row, credparity.Plan, booked, func(s credparity.Spend) string {
				if s.Refused() || s.Slot == "" {
					return ""
				}
				return credparity.Fingerprint(s.Slot)
			})
		})
	}
}

// parityRowSupervisor names a supervisor row's declared supervisor.
const parityRowSupervisor = "parity_supervisor"

// supervisorParityMeter is the pre-flight's key for a supervisor row: the
// supervisor declared alone, walked by preflightSupervisors — the spawn's own
// list (runview.FundedSupervisorSpecs), which must keep it exactly when a
// credential funds it — and keyed by supervisorKey.
func supervisorParityMeter(t *testing.T, ctx context.Context, row credparity.Row, msg *queue.RunMessage, keys runCredKeys) string {
	t.Helper()
	t.Setenv("ITERION_SUPERVISORS", "")
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "")
	wf := &ir.Workflow{
		Name:        "parity",
		Nodes:       map[string]ir.Node{},
		Supervisors: []*ir.Supervisor{{Name: parityRowSupervisor, Model: row.Node.Model}},
	}
	spawned := runview.FundedSupervisorSpecs(ctx, supervise.SpecsFromWorkflow(wf, nil, iterlog.Nop()), msg.ContentBoundary, msg.RunnerPool, iterlog.Nop())
	if (len(spawned) == 1) == row.Want.Refused() {
		t.Errorf("the spawn keeps %d supervisor(s) for a row whose Want is %+v — it keeps the specs the resolver funds", len(spawned), row.Want)
	}
	got := "not metered"
	for _, sup := range preflightSupervisors(ctx, wf, msg, nil, nil, iterlog.Nop()) {
		if key, metered := keys.supervisorKey(ctx, sup.spec, sup.hint); metered {
			got = key
		}
	}
	return got
}

// parityObservation feeds em the usage events the row's call leaves and
// returns the route the emitter books them under. A claw call writes the
// arm's Spend for the leg it spends onto its step events (ADR-161 decision
// 8), asked where the row's call site makes it (model.CallSite.Call: the
// run's sandbox for a dispatch, the host for a direct generation): the step
// carries the credential keys the arm answers there. A backend whose events
// carry no keys leaves a delegate event naming its route alone.
func parityObservation(t *testing.T, ctx context.Context, row credparity.Row, view model.RouteView, em *metricsEmitter) routeKey {
	t.Helper()
	leg := row.Want.Leg
	spec := row.Node.Spec(leg)
	if row.Node.Backend == delegate.BackendClaw {
		var site *model.CallSite
		for _, s := range model.CallSites(view, nil) {
			if s.ID == row.Node.SiteID() {
				site = &s
			}
		}
		if site == nil {
			t.Fatalf("model.CallSites names no site %q for the row's node", row.Node.SiteID())
		}
		// A claw node naming no model sends the detector's pick.
		base, err := site.Base(model.DetectedClawSpecUnder(row.Run.Allowlist))
		if err != nil {
			t.Fatalf("the detector's pick is refused for the leg the row spends: %v", err)
		}
		spec = site.LegSpec(leg, base)
		data := map[string]any{"declared_model": spec}
		em.observe(store.Event{Type: store.EventDelegateStarted, NodeID: row.Node.ID, Data: data})
		spend, err := credroute.Resolve(ctx, site.Call(leg, spec, credroute.Sandbox{Contained: row.Sandboxed}))
		if err != nil {
			t.Fatalf("the claw arm refuses leg %d, the leg the row spends: %v", leg, err)
		}
		step := map[string]any{"input_tokens": 1000, "output_tokens": 100}
		credroute.PutEventKeys(step, spend)
		em.observe(store.Event{Type: store.EventLLMStepFinished, NodeID: row.Node.ID, Data: step})
	} else {
		em.observe(store.Event{Type: store.EventDelegateStarted, NodeID: row.Node.ID, Data: map[string]any{"declared_model": spec}})
		em.observe(store.Event{Type: store.EventDelegateFinished, NodeID: row.Node.ID, Data: map[string]any{
			"backend": row.Node.Backend, "tokens": 1100, "cost_usd": 0.01, "route_model": spec,
		}})
	}
	routes := em.RouteTotals()
	if len(routes) != 1 {
		t.Fatalf("the row's call booked %d routes: %v", len(routes), routes)
	}
	for route := range routes {
		return route
	}
	return routeKey{}
}

// applyParityPosture blanks every credential name the readers read, applies
// the row's posture, points CLAUDE_CONFIG_DIR at a login-less dir and writes
// the run's forfait, returning its dir ("" when the run holds none).
func applyParityPosture(t *testing.T, row credparity.Row) string {
	t.Helper()
	for _, name := range credparity.PostureNames {
		t.Setenv(name, "")
	}
	for name, v := range row.Posture {
		t.Setenv(name, v)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if !row.Run.Forfait {
		return ""
	}
	dir := t.TempDir()
	if err := credparity.WriteForfait(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}
