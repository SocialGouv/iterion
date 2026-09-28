package unparse_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// #1612: a value holding a newline gets the multi-line form in a strict
// render too — the `key: |` block scalar at a property line, the backtick
// raw string for a value the block scalar cannot carry — instead of one
// escaped `"…\n…"` line.
func TestStrictRenderWritesBlockScalars(t *testing.T) {
	src := "dsl: 2\n\ntool t:\n  command: |\n    echo a\n    echo b\n\nworkflow w:\n  entry: t\n  t -> done\n"
	pr := parser.Parse("t.bot", src)
	if parseErrsOf(pr) != "" {
		t.Fatalf("fixture: %s", parseErrsOf(pr))
	}
	text := unparse.Unparse(pr.File)
	if !strings.Contains(text, "command: |\n    echo a\n    echo b\n") {
		t.Fatalf("the block scalar did not survive the render:\n%s", text)
	}
	if err := unparse.Verify(pr.File, text); err != nil {
		t.Fatalf("Verify: %v\n%s", err, text)
	}
	// A fixed point: the render of the render is the render.
	pr2 := parser.Parse("t.bot", text)
	if again := unparse.Unparse(pr2.File); again != text {
		t.Fatalf("not a fixed point:\n%s\n---\n%s", text, again)
	}
}

// The block scalar carries what the raw string cannot (a backtick) and the
// raw string carries what the block scalar cannot (no trailing newline); a
// value neither carries — a backtick AND no trailing newline — folds the
// whole file back to the one-line escapes, all or nothing (canon.Folds
// reads a spread value as proof that nothing folded).
func TestMultiLineFormsDivideAndFoldAllOrNothing(t *testing.T) {
	values := []struct {
		name, wantForm string
		v              string
	}{
		{"a script with a trailing newline", "block", "echo a\necho b\n"},
		{"a script with a backtick", "block", "echo `date`\n"},
		{"no trailing newline", "raw", "echo a\necho b"},
		{"a blank first line", "raw", "\necho a\n"},
		{"a whitespace-only line", "raw", "echo a\n  \necho b\n"},
		{"a line de-denting below the first", "raw", "  deep\nless\n"},
		{"a backtick and no trailing newline", "folds", "echo `a`\necho b"},
		{"a carriage return", "folds", "echo a\r\necho b\n"},
	}
	for _, tc := range values {
		t.Run(tc.name, func(t *testing.T) {
			f := &ast.File{
				Profile:   2,
				Tools:     []*ast.ToolNodeDecl{{Name: "t", Command: tc.v}},
				Workflows: []*ast.WorkflowDecl{{Name: "w", Entry: "t", Edges: []*ast.Edge{{From: "t", To: "done"}}}},
			}
			// A profile-2 render: strict from the header.
			text := unparse.Unparse(f)
			if err := unparse.Verify(f, text); err != nil {
				t.Fatalf("Verify: %v\n%s", err, text)
			}
			pr := parser.Parse("t.bot", text)
			if parseErrsOf(pr) != "" {
				t.Fatalf("does not parse back: %s\n%s", parseErrsOf(pr), text)
			}
			if got := pr.File.Tools[0].Command; got != tc.v {
				t.Fatalf("the value came back as %q, want %q\n%s", got, tc.v, text)
			}
			switch tc.wantForm {
			case "block":
				if !strings.Contains(text, "command: |\n") {
					t.Errorf("no block scalar:\n%s", text)
				}
			case "raw":
				if !strings.Contains(text, "command: `") {
					t.Errorf("no raw string:\n%s", text)
				}
			case "folds":
				// The form, not the bytes: the folded value itself holds a
				// backtick. What must be absent is the multi-line FORM.
				if strings.Contains(text, "command: |") || strings.Contains(text, "command: `") {
					t.Errorf("a value no form carries must fold the file:\n%s", text)
				}
			}
		})
	}
}

// All-or-nothing is per FILE: one value no form carries sends every value
// back to the one-line escapes, block-scalar-carriable ones included.
func TestOneUncarriableValueFoldsTheWholeFile(t *testing.T) {
	f := &ast.File{
		Tools: []*ast.ToolNodeDecl{
			{Name: "a", Command: "echo one\necho two\n"},  // block-scalar-able
			{Name: "b", Command: "echo `x`\nno trailing"}, // no form carries
		},
		Workflows: []*ast.WorkflowDecl{{Name: "w", Entry: "a", Edges: []*ast.Edge{{From: "a", To: "b"}, {From: "b", To: "done"}}}},
	}
	text := unparse.Unparse(f)
	if strings.Contains(text, "command: |") || strings.Contains(text, "command: `") {
		t.Fatalf("the render mixed its forms:\n%s", text)
	}
	if err := unparse.Verify(f, text); err != nil {
		t.Fatalf("Verify: %v\n%s", err, text)
	}
}

// A blank line right after a block scalar's body reads as the body's last
// line: the separator between two declarations is suppressed there, and a
// file tail comment follows the body without one — both round-trip.
func TestABlockScalarIsNotFedTheSeparatorBlank(t *testing.T) {
	f := &ast.File{
		Comments:  []*ast.Comment{{Text: "the file's tail", Place: ast.CommentAtEnd}},
		Tools:     []*ast.ToolNodeDecl{{Name: "t", Command: "echo a\necho b\n"}, {Name: "u", Command: "echo c\n"}},
		Workflows: []*ast.WorkflowDecl{{Name: "w", Entry: "t", Edges: []*ast.Edge{{From: "t", To: "u"}, {From: "u", To: "done"}}}},
	}
	text := unparse.Unparse(f)
	if err := unparse.Verify(f, text); err != nil {
		t.Fatalf("Verify: %v\n%s", err, text)
	}
	pr := parser.Parse("t.bot", text)
	if got := pr.File.Tools[0].Command; got != "echo a\necho b\n" {
		t.Fatalf("the separator blank was absorbed into the value: %q\n%s", got, text)
	}
	if !strings.Contains(text, "## the file's tail") {
		t.Fatalf("the tail comment is gone:\n%s", text)
	}
	pr2 := parser.Parse("t.bot", text)
	if why := ir.SameProgram(ir.Compile(pr.File), ir.Compile(pr2.File)); why != "" {
		t.Fatalf("not the same program: %s", why)
	}
	// A fixed point, tail comment and all.
	if again := unparse.Unparse(pr.File); again != text {
		t.Fatalf("not a fixed point:\n%s\n---\n%s", text, again)
	}
}

// A group body is indented after the fact: the block scalar's uniform
// indentation is exactly what survives that, where a raw string's
// continuation lines would not.
func TestAGroupMemberKeepsItsScriptOverLines(t *testing.T) {
	src := "dsl: 2\n\ngroup g:\n  tool t:\n    command: |\n      echo a\n      echo b\n  t -> done\n\nuse g as r\n\nworkflow w:\n  entry: r.t\n  r.t -> done\n"
	pr := parser.Parse("g.bot", src)
	if parseErrsOf(pr) != "" {
		t.Fatalf("fixture: %s", parseErrsOf(pr))
	}
	text := unparse.Unparse(pr.File)
	if !strings.Contains(text, "command: |\n      echo a\n      echo b\n") {
		t.Fatalf("the group member's block scalar did not survive:\n%s", text)
	}
	if err := unparse.Verify(pr.File, text); err != nil {
		t.Fatalf("Verify: %v\n%s", err, text)
	}
}

// A file whose LAST line is a block scalar's body takes its tail comment
// without the usual blank line between: the blank would be read into the
// value on the next parse.
func TestABlockScalarAtFileEndKeepsItsTailComment(t *testing.T) {
	f := &ast.File{
		Comments: []*ast.Comment{{Text: "the file's tail", Place: ast.CommentAtEnd}},
		Tools:    []*ast.ToolNodeDecl{{Name: "t", Command: "echo a\necho b\n"}},
	}
	text := unparse.Unparse(f)
	if err := unparse.Verify(f, text); err != nil {
		t.Fatalf("Verify: %v\n%s", err, text)
	}
	if !strings.Contains(text, "## the file's tail") {
		t.Fatalf("the tail comment is gone:\n%s", text)
	}
	pr := parser.Parse("t.bot", text)
	if got := pr.File.Tools[0].Command; got != "echo a\necho b\n" {
		t.Fatalf("the tail's blank was absorbed into the value: %q\n%s", got, text)
	}
	if again := unparse.Unparse(pr.File); again != text {
		t.Fatalf("not a fixed point:\n%s\n---\n%s", text, again)
	}
}

// Profile 1 keeps its plain values in the forms it always had — a raw
// string for a value over lines — but a value the raw string cannot hold
// (a backtick) no longer flips the whole file to strict-escape when the
// block scalar carries it.
func TestProfile1WritesABlockScalarForAValueTheRawStringCannotHold(t *testing.T) {
	src := "tool t:\n  command: |\n    echo `date`\n    echo done\n\nworkflow w:\n  entry: t\n  t -> done\n"
	pr := parser.Parse("t.bot", src)
	if parseErrsOf(pr) != "" {
		t.Fatalf("fixture: %s", parseErrsOf(pr))
	}
	text := unparse.Unparse(pr.File)
	if strings.Contains(text, "strict-escape") {
		t.Fatalf("the file flipped to strict-escape over a value the block scalar carries:\n%s", text)
	}
	if !strings.Contains(text, "command: |\n    echo `date`\n    echo done\n") {
		t.Fatalf("no block scalar:\n%s", text)
	}
	if err := unparse.Verify(pr.File, text); err != nil {
		t.Fatalf("Verify: %v\n%s", err, text)
	}
	// And a plain profile-1 value keeps its raw string (the writer's form
	// there is unchanged).
	f := &ast.File{
		Tools:     []*ast.ToolNodeDecl{{Name: "t", Command: "echo a\necho b"}},
		Workflows: []*ast.WorkflowDecl{{Name: "w", Entry: "t", Edges: []*ast.Edge{{From: "t", To: "done"}}}},
	}
	text = unparse.Unparse(f)
	if !strings.Contains(text, "command: `echo a\necho b`") {
		t.Fatalf("a profile-1 raw string was rewritten in another form:\n%s", text)
	}
	if err := unparse.Verify(f, text); err != nil {
		t.Fatalf("Verify: %v\n%s", err, text)
	}
}

func parseErrsOf(pr *parser.ParseResult) string {
	var out []string
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			out = append(out, d.Error())
		}
	}
	return strings.Join(out, "; ")
}
