package unparse_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// TestRoundtripExamples verifies that for every example workflow file
// (.bot): parse → unparse → re-parse → re-compile produces a
// valid workflow with the same number of nodes and edges.
func TestRoundtripExamples(t *testing.T) {
	examples, err := filepath.Glob(filepath.Join("..", "..", "..", "examples", "*.bot"))
	if err != nil {
		t.Fatal(err)
	}
	if len(examples) == 0 {
		t.Skip("no example files found")
	}

	for _, path := range examples {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			// Step 1: Parse original.
			pr1 := parser.Parse(name, string(src))
			if pr1.File == nil {
				t.Fatal("original parse returned nil File")
			}
			for _, d := range pr1.Diagnostics {
				if d.Severity == parser.SeverityError {
					t.Fatalf("original parse error: %s", d.Error())
				}
			}

			// Step 2: Compile original.
			cr1 := ir.Compile(pr1.File)
			if cr1.HasErrors() {
				for _, d := range cr1.Diagnostics {
					if d.Severity == ir.SeverityError {
						t.Fatalf("original compile error: %s", d.Error())
					}
				}
			}
			if cr1.Workflow == nil {
				t.Fatal("original compile returned nil workflow")
			}

			origNodes := len(cr1.Workflow.Nodes)
			origEdges := len(cr1.Workflow.Edges)

			// Step 3: Unparse.
			unparsed := unparse.Unparse(pr1.File)
			if unparsed == "" {
				t.Fatal("unparse returned empty string")
			}

			// Step 4: Re-parse the unparsed text.
			pr2 := parser.Parse(name+".roundtrip", unparsed)
			if pr2.File == nil {
				t.Fatalf("re-parse returned nil File.\nUnparsed:\n%s", unparsed)
			}
			for _, d := range pr2.Diagnostics {
				if d.Severity == parser.SeverityError {
					t.Fatalf("re-parse error: %s\nUnparsed:\n%s", d.Error(), unparsed)
				}
			}

			// Step 5: Re-compile.
			cr2 := ir.Compile(pr2.File)
			if cr2.HasErrors() {
				for _, d := range cr2.Diagnostics {
					if d.Severity == ir.SeverityError {
						t.Fatalf("re-compile error: %s\nUnparsed:\n%s", d.Error(), unparsed)
					}
				}
			}
			if cr2.Workflow == nil {
				t.Fatalf("re-compile returned nil workflow.\nUnparsed:\n%s", unparsed)
			}

			// Step 6: Compare node and edge counts.
			if len(cr2.Workflow.Nodes) != origNodes {
				t.Errorf("node count mismatch: original=%d, roundtrip=%d", origNodes, len(cr2.Workflow.Nodes))
			}
			if len(cr2.Workflow.Edges) != origEdges {
				t.Errorf("edge count mismatch: original=%d, roundtrip=%d", origEdges, len(cr2.Workflow.Edges))
			}

			// Step 7: Verify workflow name matches.
			if cr2.Workflow.Name != cr1.Workflow.Name {
				t.Errorf("workflow name mismatch: original=%q, roundtrip=%q", cr1.Workflow.Name, cr2.Workflow.Name)
			}
		})
	}
}

func TestEditorJSONToolInputRoundtrip(t *testing.T) {
	const src = `schema ToolInput:
  target: string

schema ToolOutput:
  status: string

tool run_tests:
  command: "go test ./..."
  input: ToolInput
  output: ToolOutput

workflow main:
  entry: run_tests
  run_tests -> done
`

	pr1 := parser.Parse("tool_input.bot", src)
	if pr1.File == nil {
		t.Fatal("parse returned nil File")
	}
	for _, d := range pr1.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("parse error: %s", d.Error())
		}
	}
	if len(pr1.File.Tools) != 1 || pr1.File.Tools[0].Input != "ToolInput" {
		t.Fatalf("parser did not preserve tool input; tools=%#v", pr1.File.Tools)
	}

	data, err := ast.MarshalFile(pr1.File)
	if err != nil {
		t.Fatalf("MarshalFile failed: %v", err)
	}
	restored, err := ast.UnmarshalFile(data)
	if err != nil {
		t.Fatalf("UnmarshalFile failed: %v", err)
	}
	if len(restored.Tools) != 1 || restored.Tools[0].Input != "ToolInput" {
		t.Fatalf("JSON AST roundtrip lost tool input; tools=%#v\nJSON:\n%s", restored.Tools, data)
	}

	unparsed := unparse.Unparse(restored)
	pr2 := parser.Parse("tool_input.roundtrip.bot", unparsed)
	if pr2.File == nil {
		t.Fatalf("re-parse returned nil File\nUnparsed:\n%s", unparsed)
	}
	for _, d := range pr2.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("re-parse error: %s\nUnparsed:\n%s", d.Error(), unparsed)
		}
	}
	if len(pr2.File.Tools) != 1 || pr2.File.Tools[0].Input != "ToolInput" {
		t.Fatalf("unparse roundtrip lost tool input; tools=%#v\nUnparsed:\n%s", pr2.File.Tools, unparsed)
	}

	cr := ir.Compile(pr2.File)
	if cr.HasErrors() {
		for _, d := range cr.Diagnostics {
			if d.Severity == ir.SeverityError {
				t.Fatalf("compile error: %s\nUnparsed:\n%s", d.Error(), unparsed)
			}
		}
	}
	tool, ok := cr.Workflow.Nodes["run_tests"].(*ir.ToolNode)
	if !ok {
		t.Fatalf("compiled node run_tests has type %T, want *ir.ToolNode", cr.Workflow.Nodes["run_tests"])
	}
	if tool.InputSchema != "ToolInput" || tool.OutputSchema != "ToolOutput" {
		t.Fatalf("compiled tool schemas mismatch: input=%q output=%q", tool.InputSchema, tool.OutputSchema)
	}
}

// TestActionIdentPropsSurviveARoundTrip.
//
// `action:` and `connection:` were written BARE, while the AST is also built
// programmatically (the JSON round trip, the studio editor, a refactoring
// tool) where nothing stops a space landing in either. Unparsed bare,
// `action: "forgejo issue comment"` came back as three tokens — Action
// truncated to "forgejo", `issue` read as an unknown tool property — so a save
// turned one diagnostic into a mangled node plus one about text the author
// never wrote.
func TestActionIdentPropsSurviveARoundTrip(t *testing.T) {
	for _, tc := range []struct{ name, action, connection string }{
		{"well-formed", "forgejo.issue.comment", "forge_main"},
		{"an alias with a dash", "forgejo.issue.comment", "forge-main"},
		{"an id with a space", "forgejo issue comment", "forge_main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &ast.File{Tools: []*ast.ToolNodeDecl{{
				Name: "t", Action: tc.action, Connection: tc.connection,
			}}}
			out := unparse.Unparse(src)
			res := parser.Parse("rt.bot", out)
			for _, d := range res.Diagnostics {
				if d.Severity == parser.SeverityError {
					t.Fatalf("re-parse of\n%s\nfailed: %s", out, d.Message)
				}
			}
			if len(res.File.Tools) != 1 {
				t.Fatalf("re-parse gave %d tools:\n%s", len(res.File.Tools), out)
			}
			got := res.File.Tools[0]
			if got.Action != tc.action {
				t.Errorf("Action = %q, want %q (written as:\n%s)", got.Action, tc.action, out)
			}
			if got.Connection != tc.connection {
				t.Errorf("Connection = %q, want %q (written as:\n%s)", got.Connection, tc.connection, out)
			}
		})
	}
	// The ordinary id stays UNQUOTED: quoting every action would churn the
	// diff of every `.bot` the studio saves.
	out := unparse.Unparse(&ast.File{Tools: []*ast.ToolNodeDecl{{
		Name: "t", Action: "forgejo.issue.comment", Connection: "forge_main",
	}}})
	if !strings.Contains(out, "action: forgejo.issue.comment\n") {
		t.Errorf("a well-formed id must stay bare, got:\n%s", out)
	}
}
