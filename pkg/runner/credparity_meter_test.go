package runner

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/internal/credparity"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// TestCredParity_MeterLedgerAndPlan drives every row of ADR-161's parity table
// (internal/credparity) through the runner's three readers of a node's spend
// and judges each against the row:
//
//   - the meter: the usage-cap pre-flight's key for the node's route — the
//     routes the pre-flight enumerates (model.AnthropicWireRoutes over the
//     node's workflow), keyed by runCredKeys.routeKey on the run's sandbox;
//   - the ledger: the run slot routeSlot books the spent leg's route on;
//   - the plan: the fingerprint the fine table books that route's step on,
//     through the plan the publisher derives (model.CredentialPlan over
//     model.EffectiveProviders) and the step's provider word — on the rows
//     whose run carries a plan.
//
// A row nothing spends on books nothing: the ledger and the plan read no
// observation there.
func TestCredParity_MeterLedgerAndPlan(t *testing.T) {
	for _, row := range credparity.Rows() {
		t.Run(row.Label(), func(t *testing.T) {
			forfaitDir := applyParityPosture(t, row)
			creds := row.Run.Credentials(forfaitDir)
			ctx := secrets.WithCredentials(context.Background(), creds)
			msg := &queue.RunMessage{RunID: "run-parity", TenantID: "team-a"}
			wf := &ir.Workflow{Name: "parity", Nodes: map[string]ir.Node{row.Node.ID: &ir.AgentNode{
				BaseNode:  ir.BaseNode{ID: row.Node.ID},
				LLMFields: ir.LLMFields{Backend: row.Node.Backend, Model: row.Node.Model, Provider: row.Node.Provider},
			}}}

			keys := usageCapCredKeys(ctx, msg)
			keys.sandboxed = row.Sandboxed
			meterOf := func(s credparity.Spend) string {
				switch {
				case s.Refused():
					return "not metered"
				case s.Slot != "":
					return keys.keyForSlot(s.Slot)
				case s.Channel == credparity.PodEnv:
					return usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, secrets.FingerprintSHA256(s.Credential))
				}
				return keys.keyForSlot("")
			}
			got := "not metered"
			for _, route := range model.AnthropicWireRoutes(model.RouteView{Workflow: wf}) {
				if route.NodeID != row.Node.ID {
					continue
				}
				if key, metered := keys.routeKey(ctx, route); metered {
					got = key
				}
			}
			credparity.Check(t, row, credparity.Meter, got, meterOf)

			want, ledgerPin := row.Expect(credparity.Ledger)
			if !want.Refused() || ledgerPin != nil {
				route := routeKey{backend: row.Node.Backend, model: row.Node.Spec(want.Leg)}
				credparity.Check(t, row, credparity.Ledger, routeSlot(creds, route),
					func(s credparity.Spend) string { return s.Slot })
			}

			if !row.Run.Plan {
				return
			}
			plan := model.CredentialPlan(model.EffectiveProviders(model.RouteView{Workflow: wf}, model.KnownProviderSet()), row.Run.Bundle())
			if len(plan) == 0 {
				t.Fatalf("inert bench: the publisher stamps no plan for this run")
			}
			route := routeKey{backend: row.Node.Backend, model: row.Node.Spec(want.Leg)}
			booked := newMetricsEmitter(nil, nil, plan).fingerprintFor(row.Node.ID, route, planProviderFor(route))
			credparity.Check(t, row, credparity.Plan, booked, func(s credparity.Spend) string {
				if s.Refused() || s.Slot == "" {
					return ""
				}
				return credparity.Fingerprint(s.Slot)
			})
		})
	}
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
