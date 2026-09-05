package model

import "testing"

// Ultracode on claw grants the whole orchestration surface — subagents AND
// workflows — to a node that restricts its tools, once each.
func TestWithClawOrchestrationTools(t *testing.T) {
	got := withClawOrchestrationTools([]string{"bash", "workflow"})
	has := map[string]int{}
	for _, name := range got {
		has[name]++
	}
	if has["agent"] != 1 || has["workflow"] != 1 || has["bash"] != 1 {
		t.Fatalf("tools = %v, want bash, agent and workflow exactly once each", got)
	}
}
