package bots

import (
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// On claw an empty `tools:` list is NO tools at all — not "no restriction"
// as it reads on claude_code. The claude reviewer slots shipped without a
// list, so every deployment that dials them onto claw (every sovereign pool)
// dispatched them with tool_count 0: the model could not run one git command
// and filled the output schema with a stub whose summary said "placeholder"
// while the publish step greened the gate (SocialGouv/iterion#2705, probed
// live on the vao pool). The gpt slots always carried the list; this floor
// holds all four reviewer slots to it, so a future edit cannot silently
// empty a claw-bound reviewer again.
func TestReviewPRReviewerSlotsDeclareAClawSurface(t *testing.T) {
	pr := parseBotUnit("review-pr/main.bot")
	if pr.File == nil {
		t.Fatal("parse produced no File")
	}
	cr := ir.Compile(pr.File)
	if cr.Workflow == nil {
		t.Fatal("compile produced no Workflow")
	}

	want := []string{"bash", "read_file", "glob", "grep"}
	for _, id := range []string{
		"reviewer_claude", "reviewer_claude_glance",
		"reviewer_gpt", "reviewer_gpt_glance",
	} {
		n, ok := cr.Workflow.Nodes[id]
		if !ok {
			t.Fatalf("node %q not found in the compiled bundle", id)
		}
		jn, ok := n.(*ir.JudgeNode)
		if !ok {
			t.Fatalf("node %q is %T, want a judge", id, n)
		}
		if !reflect.DeepEqual(jn.Tools, want) {
			t.Errorf("%s: tools = %v, want %v — on claw this list IS the reviewer's surface; empty means no diff was ever read (#2705)", id, jn.Tools, want)
		}
		if jn.ToolMaxSteps <= 0 {
			t.Errorf("%s: tool_max_steps = %d, want a positive bound — claw defaults a declared-tool loop far below a diff read", id, jn.ToolMaxSteps)
		}
	}
}
