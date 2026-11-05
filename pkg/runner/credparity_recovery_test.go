package runner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/internal/credparity"
	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestCredParity_StructuredRecovery drives every row of ADR-161's recovery
// table (credparity.RecoveryRows) through the real executor, its store hooks
// and the runner's meter, and judges the structured-output recovery's three
// readers against the row:
//
//   - the dispatch: the credential the recovery's request carried on the
//     wire (a stub Anthropic endpoint), and the credential keys its own
//     delegate event carries, which must be the claw arm's answer for the
//     call (credroute.Resolve, the auxiliary role, on the detector's pick);
//     a refused recovery sends nothing and leaves no event, and its answer
//     is the arm's refusal;
//   - the ledger: the run slot ledgerSlot books the recovery's observation on;
//   - the plan: the fingerprint the fine table books it on, through the plan
//     the publisher derives for the node — on the rows whose run carries one;
//     the recovery is never a plan miss and never a disagreement, since the
//     plan predicts the node's own routes, not the detector's pick.
//
// The recovery's event is booked alone, on a fresh meter: it is the
// observation under judgement, the node's own answers are not.
func TestCredParity_StructuredRecovery(t *testing.T) {
	for _, row := range credparity.RecoveryRows() {
		t.Run(row.Label(), func(t *testing.T) {
			forfaitDir := applyParityPosture(t, row)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("TMPDIR", t.TempDir())
			t.Setenv("ITERION_OUTPUT_CORRECTION_BUDGET", "2")
			wire := newRecoveryWire(t)
			creds := row.Run.Credentials(forfaitDir)
			ctx := secrets.WithCredentials(model.WithRunID(context.Background(), "run-recovery"), creds)

			sink := &recoverySink{}
			b := &r8Backend{name: row.Node.Backend, calls: []r8Call{
				{prose: "I approve: verdict true, reason fine.", tokens: 100, cost: 1},
				{prose: "Really, I approve it.", tokens: 100, cost: 1},
			}}
			e := r8Executor(r8Node(row.Node.Backend, row.Node.Model), &r8Gate{tag: "recovery", shared: &r8Shared{}, inner: sink}, model.NewRegistry(), b)
			_, runErr := e.Execute(ctx, r8Node(row.Node.Backend, row.Node.Model), map[string]any{})

			var extraction []store.Event
			for _, evt := range sink.snapshot() {
				if reask, _ := evt.Data["reask"].(string); reask == model.ReaskExtract {
					extraction = append(extraction, evt)
				}
			}
			sent := wire.snapshot()

			if row.Want.Refused() {
				if len(sent) != 0 || len(extraction) != 0 {
					t.Fatalf("a refused recovery sent %d request(s) and left %d event(s)", len(sent), len(extraction))
				}
				spec, err := model.DetectedClawSpecUnder(row.Run.Allowlist)(row.Node.ID)
				if err != nil || spec == "" {
					t.Fatalf("inert bench: the detector picks %q (%v) — the row's pod funds no pick", spec, err)
				}
				_, armErr := credroute.Resolve(ctx, credroute.Call{Backend: delegate.BackendClaw, Spec: spec, Role: credroute.RoleAuxiliary})
				var refusal *credroute.Refusal
				if !errors.As(armErr, &refusal) {
					t.Fatalf("the recovery sent nothing, yet the claw arm funds %s (%v) — the bench refused it elsewhere", spec, armErr)
				}
				credparity.Check(t, row, credparity.Dispatch, credparity.Refuses(credparity.Reason(refusal.Reason)), func(s credparity.Spend) credparity.Spend { return s })
				return
			}

			if len(sent) != 1 || len(extraction) != 1 {
				t.Fatalf("inert bench: the recovery sent %d request(s) and left %d event(s), want one of each (node error: %v)", len(sent), len(extraction), runErr)
			}
			evt := extraction[0]
			spec, _ := evt.Data["route_model"].(string)
			arm, armErr := credroute.Resolve(ctx, credroute.Call{Backend: delegate.BackendClaw, Spec: spec, Role: credroute.RoleAuxiliary})
			if armErr != nil {
				t.Fatalf("the claw arm refuses %s, which the recovery sent: %v", spec, armErr)
			}
			keys, present := credroute.SpendFromEvent(evt.Data)
			if !present || keys.Channel != arm.Channel || keys.Slot != arm.Slot || keys.Fingerprint != arm.Fingerprint {
				t.Errorf("the recovery's %s carries the credential keys %+v (present %v), want the claw arm's Spend %+v", evt.Type, keys, present, arm)
			}
			credparity.Check(t, row, credparity.Dispatch, recoveryWireSpend(t, row, sent[0]), func(s credparity.Spend) credparity.Spend { return s })

			var plan map[string]string
			if row.Run.Plan {
				node := row.Node.IR()
				plan = model.CredentialPlan(model.EffectiveProviders(model.RouteView{Workflow: r8WorkflowWith(node)}, model.KnownProviderSet()), row.Run.Bundle())
				if len(plan) == 0 {
					t.Fatalf("inert bench: the publisher stamps no plan for this run")
				}
			}
			em := newMetricsEmitter(nil, nil, plan)
			em.observe(evt)
			routes := em.RouteTotals()
			if len(routes) != 1 {
				t.Fatalf("the recovery's event booked %d routes: %v", len(routes), routes)
			}
			for route := range routes {
				slot, _ := ledgerSlot(creds, route)
				credparity.Check(t, row, credparity.Ledger, slot, func(s credparity.Spend) string { return s.Slot })
			}
			if !row.Run.Plan {
				return
			}
			rows, miss := em.fineSnapshot()
			if miss.observations > 0 {
				t.Errorf("the recovery counted as a plan miss: %+v — it voids the run's fine table", miss)
			}
			if n := em.planDisagreements(); n != 0 {
				t.Errorf("the recovery counted %d plan disagreement(s): the plan never predicted it", n)
			}
			if len(rows) > 1 {
				t.Fatalf("one recovery booked %d fine rows: %v", len(rows), rows)
			}
			booked := ""
			for fp := range rows {
				booked = fp
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

// The meter's rule for the recovery's call, on the event the store hooks
// write for it (`reask: "extract"`, claw): the plan predicts the node's
// routes, never the detector's pick. A keyed event books on its own run slot
// even where the plan predicted another fingerprint for the node's provider,
// and counts no disagreement; an event without the keys books nobody and is
// no miss — never the node's plan row by prediction. Mutant — the recovery
// joined through the plan like a node's call: the keyed event counts a
// disagreement, the keyless one books the donor's row.
func TestTheRecoveryBooksOnItsOwnKeysNeverOnThePlansPrediction(t *testing.T) {
	plan := map[string]string{"impl" + model.PlanKeySep + "anthropic": "fp-donor"}
	for _, tc := range []struct {
		name string
		keys credroute.Spend
		want string
	}{
		{"keyed on a run slot", credroute.Spend{Channel: credroute.ChannelRunKey, Slot: "anthropic", Fingerprint: "fp-own"}, "fp-own"},
		{"keyless", credroute.Spend{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			em := newMetricsEmitter(nil, nil, plan)
			data := map[string]any{
				"backend": delegate.BackendClaw, "reask": model.ReaskExtract, "provider": "anthropic",
				"route_model": "anthropic/claude-opus-5-5", "effective_model": "anthropic/claude-opus-5-5",
				"declared_model": "anthropic/claude-opus-5", "tokens": 230, "cost_usd": 0.0014,
			}
			credroute.PutEventKeys(data, tc.keys)
			em.observe(store.Event{Type: store.EventDelegateFinished, NodeID: "impl", Data: data})
			rows, miss := em.fineSnapshot()
			booked := ""
			for fp := range rows {
				booked = fp
			}
			if len(rows) > 1 || booked != tc.want {
				t.Errorf("the recovery booked the fine rows %v, want %q", rows, tc.want)
			}
			if miss.observations != 0 {
				t.Errorf("the recovery counted as a plan miss: %+v", miss)
			}
			if n := em.planDisagreements(); n != 0 {
				t.Errorf("the recovery counted %d plan disagreement(s): the plan never predicted it", n)
			}
		})
	}
}

// r8WorkflowWith is r8Workflow holding node, the workflow the publisher
// derives the node's plan from.
func r8WorkflowWith(node ir.Node) *ir.Workflow {
	wf := r8Workflow()
	wf.Nodes = map[string]ir.Node{node.NodeID(): node}
	return wf
}

// recoverySink records every event the executor's store hooks emit.
type recoverySink struct {
	mu   sync.Mutex
	evts []store.Event
}

func (s *recoverySink) AppendEvent(_ context.Context, _ string, evt store.Event) (*store.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evts = append(s.evts, evt)
	return &evt, nil
}

func (s *recoverySink) snapshot() []store.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.Event(nil), s.evts...)
}

// recoveryWire is a stub Anthropic endpoint answering the recovery's
// structured-output call, recording the credential each request carried.
type recoveryWire struct {
	mu   sync.Mutex
	sent []string
}

func newRecoveryWire(t *testing.T) *recoveryWire {
	t.Helper()
	w := &recoveryWire{}
	ts := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		cred := r.Header.Get("x-api-key")
		if cred == "" {
			cred = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		w.mu.Lock()
		w.sent = append(w.sent, cred)
		w.mu.Unlock()
		const message = `{"id":"msg_recovery","type":"message","role":"assistant","model":"claude","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":200,"output_tokens":1}}`
		rw.Header().Set("content-type", "text/event-stream")
		for _, event := range [][2]string{
			{"message_start", `{"type":"message_start","message":` + message + `}`},
			{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_recovery","name":"structured_output","input":{}}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"verdict\":true,\"reason\":\"extracted\"}"}}`},
			{"content_block_stop", `{"type":"content_block_stop","index":0}`},
			{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":30}}`},
			{"message_stop", `{"type":"message_stop"}`},
		} {
			_, _ = fmt.Fprintf(rw, "event: %s\ndata: %s\n\n", event[0], event[1])
		}
	}))
	t.Cleanup(ts.Close)
	t.Setenv("ANTHROPIC_BASE_URL", ts.URL)
	return w
}

func (w *recoveryWire) snapshot() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.sent...)
}

// recoveryWireSpend names the credential a request carried by the channel of
// the row that holds its value.
func recoveryWireSpend(t *testing.T, row credparity.Row, cred string) credparity.Spend {
	t.Helper()
	switch {
	case cred == "":
		return credparity.Refuses(credparity.NoCredential)
	case cred == credparity.ForfaitToken && row.Run.Forfait:
		return credparity.Spent(0, credparity.RunForfait, string(secrets.OAuthKindClaudeCode), cred)
	}
	for p, k := range row.Run.Keys {
		if k == cred {
			return credparity.Spent(0, credparity.RunKey, string(p), cred)
		}
	}
	for _, v := range row.Posture {
		if v == cred {
			return credparity.Spent(0, credparity.PodEnv, "", cred)
		}
	}
	t.Fatalf("the recovery sent a credential no channel of the row holds")
	return credparity.Spend{}
}
