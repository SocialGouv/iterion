package tool

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
)

// LOW-1 (#2905): a family intersected with one of its members kept NOTHING —
// a family survived only if present on both sides, a name only if in both
// name sets — so `/a [mcp__srv__*]` then `/b [mcp__srv__tool]` refused a
// tool both sides allow. The member survives the family's scope, in both
// orders; a family survives a family that covers it whole.
func TestCommandScope_IntersectKeepsAMemberTheFamilyCovers(t *testing.T) {
	// Family first, member second.
	s := &CommandScope{}
	s.Apply("a", []string{"mcp__srv__*"})
	s.Apply("b", []string{"mcp__srv__tool"})
	if err := s.CheckAllowed("mcp_srv_tool"); err != nil {
		t.Errorf("the member is inside the family's scope — it must survive the intersection: %v", err)
	}
	if err := s.CheckAllowed("mcp_srv_other"); err == nil {
		t.Error("the family's OTHER members were never in /b's list — intersect-only must not widen back to the family")
	}

	// Member first, family second: the same answer.
	r := &CommandScope{}
	r.Apply("b", []string{"mcp__srv__tool"})
	r.Apply("a", []string{"mcp__srv__*"})
	if err := r.CheckAllowed("mcp_srv_tool"); err != nil {
		t.Errorf("order must not matter — the member survives: %v", err)
	}
	if err := r.CheckAllowed("mcp_srv_other"); err == nil {
		t.Error("member-then-family must not widen to the family either")
	}

	// Family ∩ same family stands; family ∩ another server stays empty.
	f := &CommandScope{}
	f.Apply("a", []string{"mcp__srv__*"})
	f.Apply("c", []string{"mcp__srv__*", "Read"})
	if err := f.CheckAllowed("mcp_srv_anything"); err != nil {
		t.Errorf("the family is on both sides — it survives: %v", err)
	}
	g := &CommandScope{}
	g.Apply("a", []string{"mcp__srv__*"})
	g.Apply("d", []string{"mcp__other__*"})
	if err := g.CheckAllowed("mcp_srv_tool"); err == nil {
		t.Error("disjoint families intersect to nothing")
	}
	if err := g.CheckAllowed("mcp_other_tool"); err == nil {
		t.Error("disjoint families intersect to nothing")
	}
}

// LOW-3 (#2905): withAlwaysAllowed must not write the trio into the
// RECEIVER's set — maps are references, so the value receiver mutated the
// map it was handed. CommandNarrowedSet's fresh build made it harmless; a
// second caller reusing a set would have inherited the widening in silence.
func TestCommandScope_WithAlwaysAllowedDoesNotMutateTheReceiver(t *testing.T) {
	set := buildNarrowedSet([]string{"Read"})
	widened := set.withAlwaysAllowed()
	// The copy carries the member AND the trio…
	for _, name := range []string{"read_file", "command", "tool_search", "todo_write"} {
		if !widened.allows(name) {
			t.Errorf("the widened copy lost %q", name)
		}
	}
	// …and the receiver is byte-for-byte the set it was.
	for _, name := range []string{"command", "tool_search", "todo_write"} {
		if set.allows(name) {
			t.Errorf("the trio leaked into the receiver's set (%q) — the gate exempts the trio itself, the SET must not carry it", name)
		}
	}
	if !set.allows("read_file") {
		t.Error("the receiver lost its own member")
	}
}

// LOW-2 (#2905): a generation bounded by a narrowing its conversation does
// not show — a retry replaying from the bare prompt, a resume whose
// checkpoint compaction summarised the invocation away — gets the bound
// re-stated, or its next refusal names a command the model never invoked.
func TestEnsureNarrowingNotice(t *testing.T) {
	user := func(text string) api.Message {
		return api.Message{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: text}}}
	}

	// No scope, or an inactive one: nothing added.
	plain := []api.Message{user("go")}
	if got := EnsureNarrowingNotice(context.Background(), plain); len(got) != 1 {
		t.Error("a scopeless context must pass the conversation through")
	}
	idle := &CommandScope{}
	if got := EnsureNarrowingNotice(WithCommandScope(context.Background(), idle), plain); len(got) != 1 {
		t.Error("an inactive scope must pass the conversation through")
	}

	scope := &CommandScope{}
	scope.Apply("audit", []string{"Read"})
	ctx := WithCommandScope(context.Background(), scope)

	// The notice is absent: re-stated on the LAST user turn — never a new
	// message whose role flip would unpair a resumed tool_result's turn.
	got := EnsureNarrowingNotice(ctx, plain)
	if len(got) != 1 {
		t.Fatalf("the notice rides the last user turn, not a new message: %d messages", len(got))
	}
	last := got[0].Content[len(got[0].Content)-1]
	if last.Type != "text" || !strings.Contains(last.Text, narrowingNoticeMarker) {
		t.Fatalf("the re-statement carries the marker: %+v", last)
	}
	if !strings.Contains(last.Text, `"audit"`) && !strings.Contains(last.Text, "/audit") {
		t.Errorf("the re-statement names the command that narrowed: %q", last.Text)
	}
	if !strings.Contains(last.Text, "earlier in this node execution") {
		t.Errorf("the re-statement says WHY the invocation is not in view: %q", last.Text)
	}
	// The caller's slice is untouched.
	if len(plain[0].Content) != 1 {
		t.Error("the caller's message was mutated")
	}

	// Already said — flat or nested in the invocation's tool_result: no
	// repeat.
	said := []api.Message{user("go"), {Role: "user", Content: []api.ContentBlock{api.ToolResult{
		ToolUseID: "tu_1", Content: "NOTE (iterion): /audit " + narrowingNoticeMarker + " [read_file] ...",
	}.ToContentBlock()}}}
	if got := EnsureNarrowingNotice(ctx, said); len(got) != 2 || len(got[1].Content) != 1 {
		t.Error("the notice already rides the conversation — a repeat is noise")
	}

	// A tail that is NOT a user turn (a resumed mid-loop state) gets the
	// notice as a fresh user message — the alternation stays valid.
	tailed := []api.Message{user("go"), {Role: "assistant", Content: []api.ContentBlock{{Type: "text", Text: "working"}}}}
	got = EnsureNarrowingNotice(ctx, tailed)
	if len(got) != 3 || got[2].Role != "user" || !strings.Contains(got[2].Content[0].Text, narrowingNoticeMarker) {
		t.Fatalf("an assistant tail gets a fresh user message: %+v", got)
	}
}
