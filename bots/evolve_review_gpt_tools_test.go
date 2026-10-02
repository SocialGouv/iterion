package bots

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// bots/evolve's gpt reviewer must hold NO tools: its model sits near a
// ChatGPT-forfait context window and one or two read_file results pushed the
// second request over it (context_length_exceeded, 2026-06-24 dogfood).
//
// The rule lived in a ten-line comment and in a `tools: []` line that a
// formatting pass deleted — an empty list used to parse as an absent one, so
// the line said nothing and dropping it changed nothing (#1615). This test is
// what makes the line load-bearing: it reddens both when the declaration is
// removed (undeclared = the CLI backends' full native toolset) and when a
// tool is added to it without the comment being rewritten.
func TestEvolveGPTReviewerDeclaresNoTools(t *testing.T) {
	pr := parseBotUnit("evolve/main.bot")
	if pr.File == nil {
		t.Fatal("bots/evolve/main.bot does not parse")
	}
	cr := ir.Compile(pr.File)
	if cr.Workflow == nil {
		t.Fatal("bots/evolve/main.bot does not compile")
	}
	n, ok := cr.Workflow.Nodes["review_gpt"].(ir.LLMNode)
	if !ok {
		t.Fatal("evolve has no review_gpt LLM node")
	}
	tools := n.GetTools()
	if !toolcatalog.ToolsDeclared(tools) {
		t.Fatal("review_gpt declares no tools: list — an undeclared surface is the backend's FULL toolset, which is what the node's own comment forbids; write `tools: []`")
	}
	if len(tools) != 0 {
		t.Fatalf("review_gpt holds %v — the node judges the vision on its textual merits; adding a tool needs the comment above it rewritten first", tools)
	}
	if backend := n.GetLLMFields().Backend; backend != "claw" {
		t.Fatalf("review_gpt runs on %q; the empty declaration is only enforced on a backend that receives the list", backend)
	}
}
