package runtime

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// Every arm of a simulation is off on an engine nobody asked to simulate,
// and on when asked: a production launch never passes the option, so a
// wait on the world stays a wait there.
func TestAProductionEngineSimulatesNothing(t *testing.T) {
	if New(&ir.Workflow{}, nil, nil).Simulating() {
		t.Fatal("an engine built without WithSimulation simulates")
	}
	for name, s := range map[string]Simulation{
		"humans":  {AnswerHumans: true},
		"events":  {EventsArrive: true},
		"answers": {AnswersArrive: true},
	} {
		if !New(&ir.Workflow{}, nil, nil, WithSimulation(s)).Simulating() {
			t.Fatalf("the %s arm does not read as simulating", name)
		}
	}
	if New(&ir.Workflow{}, nil, nil, WithSimulation(Simulation{})).Simulating() {
		t.Fatal("the zero simulation reads as simulating")
	}
}
