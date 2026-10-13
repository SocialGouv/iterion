package delegate

import (
	"strings"
	"testing"
)

// TestCapabilityRefusalWithoutANodeReadsAsASentence covers the refusal a TOOL
// raises: the executor that returns it is registered once per run, not per
// node, so it has no node id to name — and the message is what the model
// reads as its tool error. The node is named where it is known (the run log's
// tool-call hook); the sentence must not claim a node called "".
func TestCapabilityRefusalWithoutANodeReadsAsASentence(t *testing.T) {
	err := &ErrCapabilityUnsupported{
		Backend:    "claw",
		Capability: "the `agent` tool",
		Remedy:     "do the work in this node",
	}
	got := err.Error()
	if strings.Contains(got, `node ""`) {
		t.Errorf("Error() = %q; an empty node id must not be printed as a node name", got)
	}
	for _, want := range []string{"claw", "the `agent` tool", "do the work in this node"} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() = %q, missing %q", got, want)
		}
	}
}

// TestCapabilityRefusalWithANodeStillNamesIt pins the shape every dispatch-time
// refusal already prints, so the empty-node branch cannot be bought by losing
// the node id where there is one.
func TestCapabilityRefusalWithANodeStillNamesIt(t *testing.T) {
	err := &ErrCapabilityUnsupported{NodeID: "asker", Backend: "codex", Capability: "interaction: async"}
	got := err.Error()
	for _, want := range []string{`node "asker"`, `backend "codex"`, "interaction: async",
		"choose a backend and transport that support this capability"} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() = %q, missing %q", got, want)
		}
	}
}
