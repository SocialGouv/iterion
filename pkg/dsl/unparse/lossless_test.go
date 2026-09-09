package unparse_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/internal/dsltest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// The unparser is the studio's save path: every string value must come
// back from parse(unparse(f)) as exactly the value f held — a backslash
// sequence, a quote, a newline, a backtick — or a canvas edit of an
// unrelated field silently rewrites a tool command. Each shape below is a
// value the v1 lexer reads differently from Go's %q; the last ones have no
// v1 form at all and switch the file to strict-escape mode.
func TestUnparseKeepsEveryStringShape(t *testing.T) {
	values := []string{
		"plain",
		"with 'single' quotes",
		`a literal \n backslash sequence`,
		`a trailing backslash \`,
		`say "hi"`,
		"two\nlines",
		"tab\there",
		"printf %s \"line1\nline2\"\necho second",
		"uses `backticks` only",
		"backticks `and` \"quotes\"",
		"backticks `and`\nnewlines",
		"backticks `and` a \\ backslash",
		"unicode — é ✓",
		"trailing newline\n",
		"\nleading newline",
		"  leading spaces",
	}
	for _, v := range values {
		t.Run(v, func(t *testing.T) {
			f := &ast.File{
				Tools: []*ast.ToolNodeDecl{{Name: "t", Command: v, Description: v}},
				Workflows: []*ast.WorkflowDecl{{
					Name: "w", Entry: "t",
					Edges: []*ast.Edge{{From: "t", To: "done", With: []*ast.WithEntry{{Key: "note", Value: v}}}},
				}},
			}
			text := unparse.Unparse(f)
			pr := parser.Parse("shape.bot", text)
			for _, d := range pr.Diagnostics {
				if d.Severity == parser.SeverityError {
					t.Fatalf("does not parse back: %s\n%s", d.Error(), text)
				}
			}
			if got := pr.File.Tools[0].Command; got != v {
				t.Errorf("command: %q came back as %q\n%s", v, got, text)
			}
			if got := pr.File.Tools[0].Description; got != v {
				t.Errorf("description: %q came back as %q\n%s", v, got, text)
			}
			if got := pr.File.Workflows[0].Edges[0].With[0].Value; got != v {
				t.Errorf("with value: %q came back as %q\n%s", v, got, text)
			}
			if err := unparse.Verify(f, text); err != nil {
				t.Errorf("Verify: %v\n%s", err, text)
			}
		})
	}
}

// A value with a backtick AND a quote, newline or backslash has no v1 form:
// the file switches to strict-escape mode and says so in its first line, so
// the same value round-trips through the standard escapes.
func TestUnparseSwitchesToStrictEscapeWhenNoV1FormExists(t *testing.T) {
	f := &ast.File{
		Tools: []*ast.ToolNodeDecl{
			{Name: "a", Command: "echo `x` \"y\""},
			{Name: "b", Command: `plain with \n literal`}, // re-encoded for strict mode too
		},
		Workflows: []*ast.WorkflowDecl{{Name: "w", Entry: "a", Edges: []*ast.Edge{{From: "a", To: "b"}, {From: "b", To: "done"}}}},
	}
	text := unparse.Unparse(f)
	if !strings.HasPrefix(text, "## strict-escape: on\n") {
		t.Fatalf("expected the strict-escape directive on the first line:\n%s", text)
	}
	pr := parser.Parse("strict.bot", text)
	for _, d := range pr.Diagnostics {
		t.Fatalf("does not parse back: %s\n%s", d.Error(), text)
	}
	if got := pr.File.Tools[0].Command; got != "echo `x` \"y\"" {
		t.Errorf("value came back as %q", got)
	}
	if got := pr.File.Tools[1].Command; got != `plain with \n literal` {
		t.Errorf("the other value was re-encoded wrongly for strict mode: %q", got)
	}
	// A file that already declares strict mode keeps it, without a second directive.
	f.Comments = []*ast.Comment{{Text: "strict-escape: on"}}
	text = unparse.Unparse(f)
	if strings.Count(text, "strict-escape: on") != 1 {
		t.Errorf("directive duplicated:\n%s", text)
	}
	if err := unparse.Verify(f, text); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

// Groups and their instantiations are declarations like any other: they
// must come out of the unparser (they used to vanish), members through the
// same writers as top-level nodes, and compile to the same program.
func TestUnparseKeepsGroupsAndUses(t *testing.T) {
	src := "schema pout:\n  id: string\n  ok: bool\n\n" +
		"group gate_block(label, retries):\n" +
		"  tool gate:\n    command: `printf '{\"id\":\"%s\",\"ok\":true}' \"{{params.label}}\"`\n    output: pout\n    needs: slot\n" +
		"  tool check:\n    command: `printf '{\"id\":\"x\",\"ok\":true}'`\n    output: pout\n" +
		"  gate -> check when ok\n  gate -> check else\n\n" +
		"use gate_block as r1 with { label: \"A\", retries: \"2\" }\n" +
		"use gate_block as r2 with { label: \"B\", retries: \"3\" }\n\n" +
		"workflow w:\n  entry: r1.gate\n  resources:\n    slot: [\"s1\", \"s2\"]\n" +
		"  r1.check -> r2.gate\n  r2.check -> done\n"
	pr := parser.Parse("groups.bot", src)
	for _, d := range pr.Diagnostics {
		t.Fatalf("fixture does not parse: %s", d.Error())
	}
	direct := ir.Compile(pr.File)
	if direct.HasErrors() {
		t.Fatalf("fixture does not compile: %v", direct.Diagnostics)
	}
	text := unparse.Unparse(pr.File)
	if !strings.Contains(text, "group gate_block(label, retries):") || strings.Count(text, "use gate_block as ") != 2 {
		t.Fatalf("groups/uses did not come out:\n%s", text)
	}
	pr2 := parser.Parse("groups.bot", text)
	for _, d := range pr2.Diagnostics {
		t.Fatalf("does not parse back: %s\n%s", d.Error(), text)
	}
	dsltest.AssertSameProgram(t, "groups.bot", direct, ir.Compile(pr2.File))
	if err := unparse.Verify(pr.File, text); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

// A group body is rendered by a sub-writer and indented AFTER the fact, so
// a value spanning lines cannot go out as a raw `…` string: the
// continuation lines would be indented with it and the value would come
// back two spaces wider. The writer's `nested` flag is what refuses that
// form inside a group and flips the file to strict escapes instead — every
// shape below is one the top-level writer renders raw, so this fails the
// moment the flag stops being set.
func TestUnparseKeepsMultilineValuesInsideAGroup(t *testing.T) {
	for _, v := range []string{
		"printf one\nprintf two",
		"echo \"quoted\"\necho second",
		"leading\n  already indented\nlast",
	} {
		t.Run(v, func(t *testing.T) {
			src := "group g:\n  tool t:\n    command: \"placeholder\"\n\nuse g as r\n\nworkflow w:\n  entry: r.t\n  r.t -> done\n"
			pr := parser.Parse("nested.bot", src)
			for _, d := range pr.Diagnostics {
				t.Fatalf("fixture does not parse: %s", d.Error())
			}
			pr.File.Groups[0].Tools[0].Command = v
			direct := ir.Compile(pr.File)
			if direct.HasErrors() {
				t.Fatalf("fixture does not compile: %v", direct.Diagnostics)
			}
			text := unparse.Unparse(pr.File)
			pr2 := parser.Parse("nested.bot", text)
			for _, d := range pr2.Diagnostics {
				t.Fatalf("does not parse back: %s\n%s", d.Error(), text)
			}
			if got := pr2.File.Groups[0].Tools[0].Command; got != v {
				t.Errorf("group member command: %q came back as %q\n%s", v, got, text)
			}
			dsltest.AssertSameProgram(t, "nested.bot", direct, ir.Compile(pr2.File))
			if err := unparse.Verify(pr.File, text); err != nil {
				t.Errorf("Verify: %v\n%s", err, text)
			}
		})
	}
}

// Every workflow in the repository must survive parse → unparse → parse as
// the same program: that is what a studio save of an untouched document
// promises.
func TestCorpusSurvivesUnparse(t *testing.T) {
	checked := 0
	for _, path := range dsltest.CorpusFiles(t, filepath.Join("..", "..", "..")) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		pr := parser.Parse(path, string(src))
		skip := false
		for _, d := range pr.Diagnostics {
			if d.Severity == parser.SeverityError {
				skip = true // a deliberately broken fixture
			}
		}
		if skip || pr.File == nil {
			continue
		}
		text := unparse.Unparse(pr.File)
		pr2 := parser.Parse(path, text)
		for _, d := range pr2.Diagnostics {
			if d.Severity == parser.SeverityError {
				t.Errorf("%s: the unparsed source does not parse: %s", path, d.Error())
				skip = true
			}
		}
		if skip {
			continue
		}
		dsltest.AssertSameProgram(t, path, ir.Compile(pr.File), ir.Compile(pr2.File))
		checked++
	}
	t.Logf("checked %d workflows through the unparser", checked)
}
