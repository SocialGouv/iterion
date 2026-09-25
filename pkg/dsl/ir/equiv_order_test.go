package ir

import "testing"

// firstWorkflowDifference names the node a difference is about by sorted
// id, never by a map's iteration: with several nodes missing, or several
// appeared, the same one is named on every call.
func TestFirstWorkflowDifferenceNamesTheSameNodeOnEveryRun(t *testing.T) {
	nodes := func(ids ...string) map[string]Node {
		m := map[string]Node{}
		for _, id := range ids {
			m[id] = &AgentNode{BaseNode: BaseNode{ID: id}}
		}
		return m
	}
	many := &Workflow{Nodes: nodes("zeta", "mid", "alpha", "omega")}
	none := &Workflow{Nodes: nodes()}
	for i := 0; i < 30; i++ {
		if got := firstWorkflowDifference(many, none); got != `node "alpha" is missing` {
			t.Fatalf("call %d: %s, want the first missing node by id", i, got)
		}
		if got := firstWorkflowDifference(none, many); got != `node "alpha" appeared` {
			t.Fatalf("call %d: %s, want the first appeared node by id", i, got)
		}
	}
}
