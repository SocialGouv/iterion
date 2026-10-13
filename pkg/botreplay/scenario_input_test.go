package botreplay

import (
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// A scenario's `Input` and the `input` of the fixture it recorded stand in for
// what the runtime hands the node: record mode passes the map straight to
// Execute (record.go), and the committed fixture is the only description of
// that input anyone reads afterwards. Nothing else checks it — VerifySchema
// validates the recorded OUTPUT against the node's output schema and no more —
// so a field dropped from a node's input schema leaves both halves claiming
// the node still receives it. Record mode writes the scenario map back as the
// fixture's input (record.go), so the stale shape returns the next time a
// fixture is recorded.
//
// The key has to be one the node can hold: `ir.NodeInputKeys` — its input
// schema's fields, every key an incoming edge maps onto it, plus, on the entry
// node, every var the launch could supply (an upper bound there: the run's
// floor itself seeds only the defaulted ones). `_`-prefixed keys are runtime plumbing
// (`_session_id` and kin), out of scope here as in the catalogue guards.
func TestScenarioInputsCarryOnlyKeysTheNodeHolds(t *testing.T) {
	scenarios := Scenarios()
	if len(scenarios) == 0 {
		t.Fatal("no scenario wired — Scenarios() is the subject of this guard")
	}

	wfCache := map[string]*ir.Workflow{}
	checked := 0
	for _, s := range scenarios {
		wf, ok := wfCache[s.Bot]
		if !ok {
			compiled, err := CompileBot(s.Bot)
			if err != nil {
				t.Fatalf("%s: compile: %v", s.Bot, err)
			}
			wf, wfCache[s.Bot] = compiled, compiled
		}
		if _, exists := wf.Nodes[s.Node]; !exists {
			t.Errorf("%s/%s: node %q is not in the bot — the scenario names a node the bundle no longer has", s.Bot, s.Name, s.Node)
			continue
		}
		held := ir.NodeInputKeys(wf, s.Node)

		inputs := map[string]map[string]any{"scenario " + s.Name: s.Input}
		if f, err := LoadFixture(s.FixturePath()); err == nil {
			inputs["fixture "+s.FixturePath()] = f.Input
		} else {
			t.Errorf("%s/%s: load fixture: %v", s.Bot, s.Name, err)
		}

		for origin, input := range inputs {
			var keys []string
			for k := range input {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if strings.HasPrefix(k, "_") {
					continue
				}
				checked++
				if held[k] {
					continue
				}
				t.Errorf("%s: %s hands node %q the key %q, which the node cannot hold — its input schema does not declare it and no incoming edge maps it, so the recorded input describes plumbing the bundle no longer has. Drop the key from the scenario AND from the fixture's `input`, or carry it on the node",
					s.Bot, origin, s.Node, k)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no scenario input key walked — the scenarios carry no input and this guard proves nothing")
	}
	t.Logf("checked %d recorded input key(s) over %d scenario(s), scenario map and committed fixture each", checked, len(scenarios))
}
