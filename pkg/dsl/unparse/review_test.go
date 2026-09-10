package unparse_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// A document that declares no workflow yet — every half-authored canvas
// document — has no compiled program to compare; Verify used to pass
// anything of that shape, and the studio wrote the corrupted file. The
// AST mirror is the oracle there.
func TestVerifyIsNotBlindToADocumentWithoutAWorkflow(t *testing.T) {
	f := &ast.File{
		Prompts: []*ast.PromptDecl{{Name: "p", Body: "hello"}},
		Tools:   []*ast.ToolNodeDecl{{Name: "t", Command: "echo one"}},
	}
	// Sabotage the text the way a lossy writer would: a prompt line that
	// is not the document's.
	text := strings.Replace(unparse.Unparse(f), "  hello\n", "  hullo\n", 1)
	err := unparse.Verify(f, text)
	if err == nil {
		t.Fatal("a prompt line that is not the document's is not what the text holds; Verify must say so")
	}
	if !strings.Contains(err.Error(), "prompts") && !strings.Contains(err.Error(), "document") {
		t.Errorf("the refusal does not name what changed: %v", err)
	}
	// And a document without a workflow that round-trips exactly passes.
	ok := &ast.File{Tools: []*ast.ToolNodeDecl{{Name: "t", Command: "echo one"}}}
	if err := unparse.Verify(ok, unparse.Unparse(ok)); err != nil {
		t.Errorf("an exact round-trip was refused: %v", err)
	}
}

// The lexer folds CRLF to LF before reading anything, so no v1 form carries
// a carriage return: such a value flips the file to strict mode, where the
// escape does.
func TestUnparseCarriageReturnSurvives(t *testing.T) {
	for _, v := range []string{"echo one\r\necho two", "lone\rcr", "tick ` and \r\n"} {
		f := &ast.File{Tools: []*ast.ToolNodeDecl{{Name: "t", Command: v}}}
		text := unparse.Unparse(f)
		pr := parser.Parse("cr.bot", text)
		for _, d := range pr.Diagnostics {
			t.Fatalf("%q: does not parse back: %s\n%s", v, d.Error(), text)
		}
		if got := pr.File.Tools[0].Command; got != v {
			t.Errorf("%q came back as %q\n%s", v, got, text)
		}
		if err := unparse.Verify(f, text); err != nil {
			t.Errorf("%q: Verify: %v", v, err)
		}
	}
}

// The lexer reads the directive from the first 32 lines only: a directive
// sitting at comment #35 is written on line 1 whatever its place in the
// list, and only once.
func TestUnparseHoistsTheStrictEscapeDirective(t *testing.T) {
	var comments []*ast.Comment
	for i := 0; i < 34; i++ {
		comments = append(comments, &ast.Comment{Text: "header line"})
	}
	comments = append(comments, &ast.Comment{Text: "strict-escape: on"})
	f := &ast.File{
		Comments: comments,
		Tools:    []*ast.ToolNodeDecl{{Name: "t", Command: `grep "a\.b" f`}},
	}
	text := unparse.Unparse(f)
	if !strings.HasPrefix(text, "## strict-escape: on\n") {
		t.Fatalf("directive not on line 1:\n%s", text[:80])
	}
	if strings.Count(text, "strict-escape: on") != 1 {
		t.Errorf("directive written more than once:\n%s", text)
	}
	pr := parser.Parse("late.bot", text)
	for _, d := range pr.Diagnostics {
		t.Fatalf("does not parse back: %s", d.Error())
	}
	if got := pr.File.Tools[0].Command; got != `grep "a\.b" f` {
		t.Errorf("value came back as %q (read in the wrong mode)", got)
	}
	if err := unparse.Verify(f, text); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

// A node added on the canvas and saved before it is filled in has no
// properties; a header with no indented body does not parse. It gets a
// no-op body the next save drops.
func TestUnparseGivesAnEmptyDeclarationABody(t *testing.T) {
	f := &ast.File{
		Agents:       []*ast.AgentDecl{{Name: "a"}},
		Tools:        []*ast.ToolNodeDecl{{Name: "t"}},
		Computes:     []*ast.ComputeDecl{{Name: "c"}},
		Emits:        []*ast.EmitDecl{{Name: "e"}},
		Waits:        []*ast.WaitDecl{{Name: "w"}},
		AwaitAnswers: []*ast.AwaitAnswersDecl{{Name: "aa"}},
		Fails:        []*ast.FailDecl{{Name: "f"}},
		Subbots:      []*ast.SubbotDecl{{Name: "s"}},
		Humans:       []*ast.HumanDecl{{Name: "h"}},
		Judges:       []*ast.JudgeDecl{{Name: "j"}},
	}
	text := unparse.Unparse(f)
	pr := parser.Parse("empty.bot", text)
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("does not parse back: %s\n%s", d.Error(), text)
		}
	}
	if err := unparse.Verify(f, text); err != nil {
		t.Errorf("Verify: %v\n%s", err, text)
	}
}

// A value with a backtick beside a quote, backslash or newline has no v1
// form: the WHOLE file is rendered in strict-escape mode — every other
// quoted value re-escaped, the directive on line 1. Lossless, and worth
// knowing when a studio save of a shipped bot suddenly carries the
// directive: this is the condition.
func TestUnparseNamesTheStrictFlipCondition(t *testing.T) {
	plain := &ast.File{Tools: []*ast.ToolNodeDecl{{Name: "a", Command: `say \n literally`}, {Name: "b", Command: "x"}}}
	if text := unparse.Unparse(plain); strings.Contains(text, "strict-escape") {
		t.Fatalf("no value needs strict mode, yet the file flipped:\n%s", text)
	}
	flipped := &ast.File{Tools: []*ast.ToolNodeDecl{{Name: "a", Command: `say \n literally`}, {Name: "b", Command: "echo `x` \"y\""}}}
	text := unparse.Unparse(flipped)
	if !strings.HasPrefix(text, "## strict-escape: on\n") {
		t.Fatalf("a backtick beside a quote must flip the file:\n%s", text)
	}
	if !strings.Contains(text, `command: "say \\n literally"`) {
		t.Errorf("the other value was not re-escaped for strict mode:\n%s", text)
	}
	if err := unparse.Verify(flipped, text); err != nil {
		t.Errorf("Verify: %v", err)
	}
}
