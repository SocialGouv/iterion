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
//     says `low` from the cell when the executor knows the launch carried no
//     row, from the launch when its row asked for it, from the overrides when
//     the executor was not told which rows the launch carried;
//   - the dial alone (`max`, accepted by the feed) is sent as asked, from the
//     operator's dial; with the variable unset, the dial's default is the
//     node's own;
//   - an edge's `_reasoning_effort` is sent as asked, from the edge;
//   - a level the feed does not accept (`medium`) is replaced by the feed's
//     nearest (`high`): the output names the feed and the level asked for,
//     beside the `reasoning_effort_adjusted` event.
//
// Mutation that reddens it: drop the recordReasoningEffort call from
// executeDelegate (model_boundary.go), or the `_reasoning_effort_*` stamps
// from stampDelegateOutputMeta; read the launch rows nowhere in
// requestedEffort; drop effortDial.
func TestTheNodeOutputCarriesTheEffortTheCallRanAt(t *testing.T) {
	launchLow := func() *ModelOverrides {
		var o ModelOverrides
		o.SetEffort("judge", "low")
		return &o
	}
	for _, tc := range []struct {
		name          string
		cell          string
		launch        *ModelOverrides
		dialUnset     bool
		nodeEffort    string
		edge          string
		wantWire      string
		wantSource    string
		wantRequested string
		wantAdjusted  int
	}{
		{name: "a cell's low wins the env dial", cell: "low", launch: &ModelOverrides{}, nodeEffort: "${ITERION_VIBE_EFFORT_CLAUDE:-high}", wantWire: "low", wantSource: EffortSourceCell},
		{name: "the launch's own row", cell: "low", launch: launchLow(), nodeEffort: "${ITERION_VIBE_EFFORT_CLAUDE:-high}", wantWire: "low", wantSource: EffortSourceLaunch},
		{name: "overrides whose layer the executor was not told", cell: "low", nodeEffort: "${ITERION_VIBE_EFFORT_CLAUDE:-high}", wantWire: "low", wantSource: EffortSourceOverride},
		{name: "the env dial alone", nodeEffort: "${ITERION_VIBE_EFFORT_CLAUDE:-high}", wantWire: "max", wantSource: EffortSourceDialPrefix + "ITERION_VIBE_EFFORT_CLAUDE"},
		{name: "the dial's default", dialUnset: true, nodeEffort: "${ITERION_VIBE_EFFORT_CLAUDE:-high}", wantWire: "high", wantSource: EffortSourceNode},
		{name: "an edge's level", nodeEffort: "${ITERION_VIBE_EFFORT_CLAUDE:-high}", edge: "xhigh", wantWire: "xhigh", wantSource: EffortSourceEdge},
		{name: "a level the feed replaces", nodeEffort: "medium", wantWire: "high", wantSource: "feed:pliage", wantRequested: "medium", wantAdjusted: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r3GatewayEnv(t)
			unsetGatewayTables(t)
			t.Setenv("ITERION_VIBE_EFFORT_CLAUDE", "max")
			if tc.dialUnset {
				t.Setenv("ITERION_VIBE_EFFORT_CLAUDE", "")
			}
			ctx := feedEffortCtx(t)
			mock := newMockClient(toolUseEvents("ok", "structured_output", `{"verdict":true,"reason":"ok"}`, 10, 10))
			reg := NewRegistry()
			reg.Register("openai_compatible", func(string) (api.APIClient, error) { return mock, nil })
			emitter := &captureEmitter{}
			var overrides ModelOverrides
			if tc.cell != "" {
				overrides.SetEffort("judge", tc.cell)
			}
			opts := []ClawExecutorOption{WithLogger(iterlog.Nop()), WithModelOverrides(overrides),
				WithEventHooks(NewStoreEventHooks(ctx, emitter, "effort", iterlog.Nop(), nil, nil))}
			if tc.launch != nil {
				opts = append(opts, WithLaunchModelOverrides(*tc.launch))
			}
			e := newTestClawExecutor(reg, verdictWorkflow(), opts...)
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

// TestACellWinningAtTheLaunchRowsLevelIsTheCells names the override's layer
// by the rule that won, not by its level: a launch row on the node's kind and
// a routing cell on the node's id both say `low`; the cell's more specific
// selector decides the node, and the record says so.
//
// Mutation that reddens it: compare the levels alone in requestedEffort.
func TestACellWinningAtTheLaunchRowsLevelIsTheCells(t *testing.T) {
	var launch, folded ModelOverrides
	launch.SetEffort("judge", "low")
	folded.SetEffort("j", "low")
	folded.SetEffort("judge", "low")
	e := newTestClawExecutor(NewRegistry(), verdictWorkflow(), WithLogger(iterlog.Nop()),
		WithModelOverrides(folded), WithLaunchModelOverrides(launch))
	node := verdictJudge("claw")
	node.ID = "j"
	if effort, source := e.requestedEffort(node, "", nil); effort != "low" || source != EffortSourceCell {
		t.Errorf("requested %q from %q, want low from %q", effort, source, EffortSourceCell)
	}
	launch = ModelOverrides{}
	launch.SetEffort("j", "low")
	e = newTestClawExecutor(NewRegistry(), verdictWorkflow(), WithLogger(iterlog.Nop()),
		WithModelOverrides(folded), WithLaunchModelOverrides(launch))
	if effort, source := e.requestedEffort(node, "", nil); effort != "low" || source != EffortSourceLaunch {
		t.Errorf("control: the launch row on the winning selector reads %q from %q, want low from %q", effort, source, EffortSourceLaunch)
	}
}
