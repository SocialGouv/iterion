package parser_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A comment opens with `#`; `##` is the same comment with one more hash.
// The single form is what a YAML-trained author (human or model) writes
// first: measured on the repo's own documentation, 25 of the 48 `iter`
// fences that failed to parse did so on a `# note` the lexer used to read
// as an unknown property named '#'. The places where `#` stays text —
// inside a string, a prompt body, a block scalar — are asserted here so
// the relaxation can never eat data.
func TestHashCommentsAreComments(t *testing.T) {
	src := `# leading single-hash comment
## leading double-hash comment
vars:
  count: int = 3 # trailing after a literal
  mode: string [enum: "a", "b"] = "a"  # trailing after an enum default

schema out:
  ok: bool # trailing after a type
  # a comment line inside a block
  note: string

prompt p:
  # this line is prompt text, not a comment
  Keep the "#hashtag" and the trailing # in prose.

agent a: # trailing after the header
  model: "m#1" # the hash inside the string is data
  tools: [bash, grep] # after a list
  output: out
  system: p
  # a comment line between properties
  reasoning_effort: high

tool t:
  command: |
    echo '# not a comment inside a block scalar'
  output: out

workflow w: # header comment
  entry: a
  # comment line among edges
  a -> t # trailing after an edge
  t -> done
`
	res := parser.Parse("hash.bot", src)
	for _, d := range res.Diagnostics {
		t.Errorf("unexpected diagnostic: %s", d.Error())
	}
	f := res.File
	if f == nil {
		t.Fatal("parsed file is nil")
	}

	// Both leading comment forms are captured as top-level comments, with
	// the same text extraction.
	if len(f.Comments) < 2 {
		t.Fatalf("expected the two leading comments to be captured, got %d", len(f.Comments))
	}
	if f.Comments[0].Text != "leading single-hash comment" {
		t.Errorf("single-hash comment text = %q", f.Comments[0].Text)
	}
	if f.Comments[1].Text != "leading double-hash comment" {
		t.Errorf("double-hash comment text = %q", f.Comments[1].Text)
	}

	if f.Vars == nil || len(f.Vars.Fields) != 2 {
		t.Fatalf("vars: expected 2 fields, got %+v", f.Vars)
	}
	if len(f.Schemas) != 1 || len(f.Schemas[0].Fields) != 2 {
		t.Fatalf("schema: expected 2 fields, got %+v", f.Schemas)
	}

	// A `#` inside a prompt body is prompt text.
	if len(f.Prompts) != 1 {
		t.Fatalf("expected 1 prompt, got %d", len(f.Prompts))
	}
	body := f.Prompts[0].Body
	if !strings.Contains(body, "# this line is prompt text, not a comment") {
		t.Errorf("prompt body lost a leading-hash line: %q", body)
	}
	if !strings.Contains(body, `"#hashtag"`) || !strings.Contains(body, "trailing # in prose") {
		t.Errorf("prompt body lost inline hashes: %q", body)
	}

	// A `#` inside a string is data; the list and the properties after a
	// comment line are intact.
	if len(f.Agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(f.Agents))
	}
	a := f.Agents[0]
	if a.Model != "m#1" {
		t.Errorf("agent model = %q, want %q", a.Model, "m#1")
	}
	if len(a.Tools) != 2 {
		t.Errorf("agent tools = %v, want 2 entries", a.Tools)
	}
	if a.ReasoningEffort != "high" {
		t.Errorf("property after a comment line was lost: reasoning_effort = %q", a.ReasoningEffort)
	}

	// A `#` inside a block scalar is data.
	if len(f.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(f.Tools))
	}
	if !strings.Contains(f.Tools[0].Command, "# not a comment inside a block scalar") {
		t.Errorf("block scalar lost its hash: %q", f.Tools[0].Command)
	}

	if len(f.Workflows) != 1 || len(f.Workflows[0].Edges) != 2 {
		t.Fatalf("workflow: expected 2 edges, got %+v", f.Workflows)
	}
}
