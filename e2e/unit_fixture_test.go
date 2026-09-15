package e2e

import "testing"

// A fixture in several files compiles as the program its files make: the
// helper reads the unit, so the node a fragment declares is there.
func TestCompileFixtureReadsTheUnit(t *testing.T) {
	wf := compileFixture(t, "unit/main.bot")
	if _, ok := wf.Nodes["worker"]; !ok {
		t.Fatalf("the fragment's node is missing (%d nodes)", len(wf.Nodes))
	}
}
