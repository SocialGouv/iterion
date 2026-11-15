package model

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestTheNodeOutputCarriesTheEffortTheCallRanAt drives a gateway node on
// claw — the team gateway's route, the platform's `pliage` feed on the run's
// context — and reads the effort back from the node's OUTPUT, which is where
// a report reads it. The node's own `reasoning_effort:` is review-pr's
// reviewer dial, `${ITERION_VIBE_EFFORT_CLAUDE:-high}`, and the environment
// sets that dial to `max`: a report that read the environment would print
// `max` whatever the run sent.
//
//   - a routing cell's `low` wins the dial: the wire carries `low`, the output
//     says `low` from the override, and nothing was replaced;
//   - the dial alone (`max`, accepted by the feed) is sent as asked, from the
//     node;
//   - an edge's `_reasoning_effort` is sent as asked, from the edge;
//   - a level the feed does not accept (`medium`) is replaced by the feed's
//     nearest (`high`): the output names the feed and the level asked for,
//     beside the `reasoning_effort_adjusted` event.
//
// Mutation that reddens it: drop the recordReasoningEffort call from
// executeDelegate (model_boundary.go), or the `_reasoning_effort_*` stamps
// from stampDelegateOutputMeta.
func TestTheNodeOutputCarriesTheEffortTheCallRanAt(t *testing.T) {
	for _, tc := range []struct {
		name          string
		cell          string
		nodeEffort    string
		edge          string
		wantWire      string
		wantSource    string
		wantRequested string
		wantAdjusted  int
	}{
		{name: "a cell's low wins the env dial", cell: "low", nodeEffort: "${ITERION_VIBE_EFFORT_CLAUDE:-high}", wantWire: "low", wantSource: EffortSourceOverride},
		{name: "the env dial alone", nodeEffort: "${ITERION_VIBE_EFFORT_CLAUDE:-high}", wantWire: "max", wantSource: EffortSourceNode},
		{name: "an edge's level", nodeEffort: "${ITERION_VIBE_EFFORT_CLAUDE:-high}", edge: "xhigh", wantWire: "xhigh", wantSource: EffortSourceEdge},
		{name: "a level the feed replaces", nodeEffort: "medium", wantWire: "high", wantSource: "feed:pliage", wantRequested: "medium", wantAdjusted: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r3GatewayEnv(t)
			unsetGatewayTables(t)
			t.Setenv("ITERION_VIBE_EFFORT_CLAUDE", "max")
			ctx := feedEffortCtx(t)
			mock := newMockClient(toolUseEvents("ok", "structured_output", `{"verdict":true,"reason":"ok"}`, 10, 10))
			reg := NewRegistry()
			reg.Register("openai_compatible", func(string) (api.APIClient, error) { return mock, nil })
			emitter := &captureEmitter{}
			var overrides ModelOverrides
			if tc.cell != "" {
				overrides.SetEffort("judge", tc.cell)
			}
			e := newTestClawExecutor(reg, verdictWorkflow(), WithLogger(iterlog.Nop()), WithModelOverrides(overrides),
				WithEventHooks(NewStoreEventHooks(ctx, emitter, "effort", iterlog.Nop(), nil, nil)))
			node := verdictJudge("claw")
			node.Model = modelroute.OpenAICompatible + "/" + feedEffortWire
			node.ReasoningEffort = tc.nodeEffort
			input := map[string]any{}
			if tc.edge != "" {
				input["_reasoning_effort"] = tc.edge
			}
			out, err := e.Execute(ctx, node, input)
			if err != nil {
				t.Fatal(err)
			}
			if calls := mock.getCalls(); len(calls) != 1 || calls[0].ReasoningEffort != tc.wantWire {
				t.Fatalf("the gateway was sent %+v, want reasoning_effort %s", calls, tc.wantWire)
			}
			if got := out[OutputReasoningEffortApplied]; got != tc.wantWire {
				t.Errorf("_reasoning_effort_applied = %v, want %q (the level the wire carried)", got, tc.wantWire)
			}
			if got := out[OutputReasoningEffortSource]; got != tc.wantSource {
				t.Errorf("_reasoning_effort_source = %v, want %q", got, tc.wantSource)
			}
			got, present := out[OutputReasoningEffortRequested]
			switch {
			case tc.wantRequested == "" && present:
				t.Errorf("_reasoning_effort_requested = %v on a call sent as asked — its presence says the route replaced the level", got)
			case tc.wantRequested != "" && got != tc.wantRequested:
				t.Errorf("_reasoning_effort_requested = %v, want %q", got, tc.wantRequested)
			}
			if _, leaked := out["_reasoning_effort"]; leaked {
				t.Errorf("the output carries `_reasoning_effort`, the INPUT key an edge raises a node's effort with")
			}
			adjusted := 0
			for _, event := range emitter.events {
				if event.Type == store.EventReasoningEffortAdjusted {
					adjusted++
				}
			}
			if adjusted != tc.wantAdjusted {
				t.Errorf("reasoning_effort_adjusted events = %d, want %d", adjusted, tc.wantAdjusted)
			}
		})
	}
}
