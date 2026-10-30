package bundlelint_test

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundlelint"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestAGateRefusedWriterIsNotCreditedThroughAnotherRoute is the iterion#2881
// witness at the warning's own level: a writer HELD on one route and REFUSED
// there by the permission gate is no write path, whatever another route's
// spelling of the same concept survives.
//
// The shape is the ticket's: a claude_code node whose `tools:` list names no
// writer, the auto-memory grant waiting on the claw fallback, and a gate
// whose allow glob covers the claude_code spelling (`Write*`) and not the
// claw one (`write_file`). Asked over the UNION of both routes' spellings,
// the gate leaves `Write` and the bundle is credited a write path no run can
// take — the gate-refused-writer bundle went silent. Asked PER ROUTE, the
// claw route's own spelling is refused and C274 stands.
func TestAGateRefusedWriterIsNotCreditedThroughAnotherRoute(t *testing.T) {
	t.Parallel()
	page := skillPage(pageTeaching(durableStoreTools(t)[0]))

	node := &ir.AgentNode{
		BaseNode:          ir.BaseNode{ID: "nexie"},
		LLMFields:         ir.LLMFields{Backend: "claude_code"},
		Tools:             []string{"read_file"},
		AutoMemory:        "on",
		InteractionFields: ir.InteractionFields{Interaction: ir.InteractionNone},
		Fallbacks:         []ir.Fallback{{Name: "alt", Backend: "claw", Model: "provider/model-a"}},
	}
	build := func(allow []string) *ir.Workflow {
		w := wf("probe", nil, nil, nil, node)
		w.Permission, w.PermissionAllow = "deny", allow
		return w
	}

	// The divergence, pinned at the predicate level: the union question
	// leaves the writer, the holding route's own question refuses it. If
	// this pair ever agrees, the witness below asserts nothing.
	union := build([]string{"Write*"})
	if !bundlelint.PermissionGateLeaves(node, union, "write_file") {
		t.Fatal("the UNION question refuses the writer — the divergence this test is about is gone")
	}
	if bundlelint.PermissionGateLeavesOnRoute(node, union, "claw", "write_file") {
		t.Fatal("the gate leaves the writer on the claw route that HOLDS it — the refusal this test is about is not exercised")
	}

	if got := durableMemoryDiags(union, page); len(got) != 1 {
		t.Errorf("a writer refused by the gate on the route that holds it was credited through another route's spelling: got %d durable-memory diagnostics, want 1: %+v", len(got), got)
	}

	// The control: the gate that leaves the claw route's own spelling too is
	// the bundle WITH the write path, and it must stay quiet — the warning
	// is the gate's verdict, not the fallback's presence.
	if got := durableMemoryDiags(build([]string{"Write*", "write_*"}), page); len(got) != 0 {
		t.Errorf("the gate leaves the writer on the route that holds it, and the bundle still draws %+v", got)
	}
}
