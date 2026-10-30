package bundlelint_test

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
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

// TestReadonlyWithholdsWhatTheBackendWithholds is the iterion#2882
// `readonly:` arm: no reading of the flag lived in this check, so
// `readonly: true` beside `tools: [write_file]` on claude_code was credited
// as the write path the spawn withholds. The per-backend fact is
// toolcatalog.ReadonlyWithholdsDurableWrite's; these rows hold the warning
// to it end to end.
func TestReadonlyWithholdsWhatTheBackendWithholds(t *testing.T) {
	t.Parallel()
	page := skillPage(pageTeaching(durableStoreTools(t)[0]))
	node := func(backend string, readonly bool, tools []string) ir.Node {
		return &ir.AgentNode{
			BaseNode:  ir.BaseNode{ID: "worker"},
			LLMFields: ir.LLMFields{Backend: backend, Readonly: readonly},
			Tools:     tools,
		}
	}
	cases := []struct {
		what     string
		backend  string
		readonly bool
		tools    []string
		want     int
	}{
		// The ticket's own shape: the declared writer is withheld by the
		// flag on claude_code, so the bundle has no write path.
		{"readonly + a declared writer on claude_code", "claude_code", true, []string{"write_file"}, 1},
		{"the same node without the flag keeps its writer", "claude_code", false, []string{"write_file"}, 0},
		// The shell is KEPT on claude_code — the flag is not a sandbox
		// there — so the unrestricted roster still writes durably.
		{"readonly + an undeclared list on claude_code keeps the shell", "claude_code", true, nil, 0},
		// codex answers the flag with a read-only OS sandbox and pi pins the
		// tool list to readers: no durable write survives either, shell
		// included.
		{"readonly + an undeclared list on codex (read-only OS sandbox)", "codex", true, nil, 1},
		{"readonly + an undeclared list on pi (readers only)", "pi", true, nil, 1},
		// claw ignores the flag entirely — documented behaviour, pinned so a
		// backend that learns the lever flips its row on purpose.
		{"readonly + a declared writer on claw, which ignores the flag", "claw", true, []string{"write_file"}, 0},
	}
	for _, c := range cases {
		got := durableMemoryDiags(wf("probe", nil, nil, nil, node(c.backend, c.readonly, c.tools)), page)
		if len(got) != c.want {
			t.Errorf("%s: got %d durable-memory diagnostics, want %d: %+v", c.what, len(got), c.want, got)
		}
	}
}

// TestADeclaredListThatNeverReachesTheBackendBoundsNothing is the
// iterion#2882 ReceivesToolList arm: pi, kimi, grok and opencode are driven
// through the CLI-agent seam, which never passes the `tools:` list to the
// agent — so a declared writer-free list is a bound their run does not have,
// and reading it as one fired C274 on bundles whose runs keep the native
// writers.
func TestADeclaredListThatNeverReachesTheBackendBoundsNothing(t *testing.T) {
	t.Parallel()
	page := skillPage(pageTeaching(durableStoreTools(t)[0]))
	node := func(backend string) ir.Node {
		return &ir.AgentNode{
			BaseNode:  ir.BaseNode{ID: "worker"},
			LLMFields: ir.LLMFields{Backend: backend},
			Tools:     []string{"read_file"},
		}
	}

	// The precondition, from the capability registry itself: none of these
	// backends receives the list. A backend that learns to reddens here
	// first, and its row below is then the wrong expectation.
	for _, backend := range []string{"pi", "kimi", "grok", "opencode"} {
		if toolcatalog.ReceivesToolList(backend) {
			t.Fatalf("%s now RECEIVES the `tools:` list — the arm this test is about is dead, and the row below must flip to the claude_code expectation", backend)
		}
		if got := durableMemoryDiags(wf("probe", nil, nil, nil, node(backend)), page); len(got) != 0 {
			t.Errorf("%s: a declared writer-free list was read as a bound this run does not have — the native roster keeps its writers: %+v", backend, got)
		}
	}

	// The control: claude_code DOES receive the list, and the same declared
	// writer-free list bounds it there.
	if got := durableMemoryDiags(wf("probe", nil, nil, nil, node("claude_code")), page); len(got) != 1 {
		t.Errorf("claude_code: the declared writer-free list must bound the surface: got %d durable-memory diagnostics, want 1", len(got))
	}

	// And the roster those routes keep is still the GATE's to bound: under
	// `permission: deny` with a writer-free allow list, no call of a native
	// writer runs on pi either.
	gated := wf("probe", nil, nil, nil, node("pi"))
	gated.Permission, gated.PermissionAllow = "deny", []string{"Read(**)"}
	if got := durableMemoryDiags(gated, page); len(got) != 1 {
		t.Errorf("pi: the unbounded roster under a writer-free gate was read as a write path: got %d durable-memory diagnostics, want 1", len(got))
	}
}
