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
		Prompts: []*ast.PromptDecl{{Name: "p", Body: "hello\n\n\n"}},
		Tools:   []*ast.ToolNodeDecl{{Name: "t", Command: "echo one"}},
	}
	text := unparse.Unparse(f)
	// Sabotage the text the way a lossy writer would: drop the prompt's
	// trailing blank lines, which changes the prompt body.
	err := unparse.Verify(f, text)
	if err == nil {
		t.Fatal("a body with trailing blank lines is not what the text holds; Verify must say so")
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

// The same for the DECLARATION kinds, which ensureBody never covered: the
// studio creates a `prompt`/`schema` the moment the button is clicked, and
// deleting a workflow's entry node leaves `workflow NAME:` with nothing under
// it. Every one of them used to render a bare header, which does not parse —
// and since this branch made Verify a hard gate on the save path, that is a
// 422 on the whole file, not a cosmetic wart.
func TestUnparseGivesAnEmptyTopLevelDeclarationABody(t *testing.T) {
	for _, tc := range []struct {
		kind string
		file *ast.File
	}{
		{"cursor", &ast.File{Cursors: []*ast.CursorDecl{{Name: "c"}}}},
		{"mcp_server", &ast.File{MCPServers: []*ast.MCPServerDecl{{Name: "m"}}}},
		{"supervisor", &ast.File{Supervisors: []*ast.SupervisorDecl{{Name: "sv"}}}},
		{"workflow", &ast.File{Workflows: []*ast.WorkflowDecl{{Name: "w"}}}},
		{"router", &ast.File{Routers: []*ast.RouterDecl{{Name: "r"}}}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			text := unparse.Unparse(tc.file)
			pr := parser.Parse("empty.bot", text)
			for _, d := range pr.Diagnostics {
				if d.Severity == parser.SeverityError {
					t.Fatalf("does not parse back: %s\n%s", d.Error(), text)
				}
			}
			if err := unparse.Verify(tc.file, text); err != nil {
				t.Errorf("Verify: %v\n%s", err, text)
			}
		})
	}
}

// A nested block the author opened and left blank is the same trap one level
// down. Two answers, and which one applies is a property of the block: a
// block whose blank form the compiler reads as no block at all is left out,
// every other one is written with a no-op property — never dropped, since
// dropping it would change the program the file describes.
func TestUnparseWritesOrDropsAnEmptyNestedBlock(t *testing.T) {
	agent := func(d ast.LLMDecl) *ast.File {
		return &ast.File{Agents: []*ast.AgentDecl{{Name: "a", LLMDecl: d}}}
	}
	for _, tc := range []struct {
		kind string
		file *ast.File
		// want is a fragment the output must carry (the no-op property),
		// or "" when the block is legitimately left out.
		want string
	}{
		{"auth", &ast.File{MCPServers: []*ast.MCPServerDecl{{Name: "m", URL: "u", Auth: &ast.MCPAuthDecl{}}}}, "auth:\n    type: \"\""},
		{"node mcp", agent(ast.LLMDecl{MCP: &ast.MCPConfigDecl{}}), "mcp:\n    servers: []"},
		{"workflow mcp", &ast.File{
			Agents:    []*ast.AgentDecl{{Name: "a"}},
			Workflows: []*ast.WorkflowDecl{{Name: "w", Entry: "a", MCP: &ast.MCPConfigDecl{}}},
		}, "mcp:\n    servers: []"},
		{"recovery", &ast.File{Tools: []*ast.ToolNodeDecl{{Name: "t", Command: "x", Recovery: &ast.RecoveryBlock{}}}}, "recovery:\n    max_repair_attempts: 0"},
		{"budget", &ast.File{Workflows: []*ast.WorkflowDecl{{Name: "w", Budget: &ast.BudgetBlock{}}}}, "budget:\n    max_iterations: 0"},
		{"compaction", agent(ast.LLMDecl{Compaction: &ast.CompactionBlock{}}), ""},
		{"memory", agent(ast.LLMDecl{Memory: &ast.MemoryBlock{}}), ""},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			text := unparse.Unparse(tc.file)
			pr := parser.Parse("empty.bot", text)
			for _, d := range pr.Diagnostics {
				if d.Severity == parser.SeverityError {
					t.Fatalf("does not parse back: %s\n%s", d.Error(), text)
				}
			}
			if tc.want != "" && !strings.Contains(text, tc.want) {
				t.Errorf("want the no-op body %q, got:\n%s", tc.want, text)
			}
			if err := unparse.Verify(tc.file, text); err != nil {
				t.Errorf("Verify: %v\n%s", err, text)
			}
		})
	}
}

// A blank `sandbox:` block is neither: it has no written form (the block
// syntax always reads back as inline mode) and dropping it changes the node,
// so it must be REFUSED — never written away in silence. This is the guard on
// the tolerance the two droppable blocks needed.
func TestVerifyRefusesADroppedSandboxBlock(t *testing.T) {
	f := &ast.File{Agents: []*ast.AgentDecl{{Name: "a", LLMDecl: ast.LLMDecl{Sandbox: &ast.SandboxBlock{}}}}}
	err := unparse.Verify(f, unparse.Unparse(f))
	if err == nil {
		t.Fatal("a sandbox block the writer dropped was accepted")
	}
	if !strings.Contains(err.Error(), "sandbox") {
		t.Errorf("the refusal does not name the block: %v", err)
	}
}

// The tolerance that lets an EMPTY compaction/memory block go unwritten must
// not extend one inch further: a block that held something and did not survive
// is a lost setting, and the guard exists to catch exactly that. Fed the text
// a lossy writer would produce, Verify has to refuse and name the block.
func TestVerifyStillCatchesABlockThatHeldSomething(t *testing.T) {
	scope := "team"
	f := &ast.File{Agents: []*ast.AgentDecl{{
		Name:    "a",
		LLMDecl: ast.LLMDecl{Memory: &ast.MemoryBlock{Scope: &scope}},
	}}}
	// What the writer would emit if it dropped a populated block.
	err := unparse.Verify(f, "agent a:\n  description: \"\"\n")
	if err == nil {
		t.Fatal("a populated memory block was dropped and the guard passed")
	}
	if !strings.Contains(err.Error(), "memory") {
		t.Errorf("the refusal does not name the lost block: %v", err)
	}
	// And the real writer keeps it, of course.
	if err := unparse.Verify(f, unparse.Unparse(f)); err != nil {
		t.Errorf("Verify: %v\n%s", err, unparse.Unparse(f))
	}
}

// A prompt with no text and a schema with no field have no written form at
// all — any placeholder would BECOME content. They are refused by NAME, the
// way an empty group already was: `expected INDENT, got EOF` about a file the
// author never sees is not an answer they can act on.
func TestVerifyNamesADeclarationTheSyntaxCannotWrite(t *testing.T) {
	for _, tc := range []struct {
		kind, want string
		file       *ast.File
	}{
		{"prompt", `prompt "p"`, &ast.File{Prompts: []*ast.PromptDecl{{Name: "p"}}}},
		{"prompt blank", `prompt "p"`, &ast.File{Prompts: []*ast.PromptDecl{{Name: "p", Body: "\n  \n"}}}},
		{"schema", `schema "s"`, &ast.File{Schemas: []*ast.SchemaDecl{{Name: "s"}}}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			err := unparse.Verify(tc.file, unparse.Unparse(tc.file))
			if err == nil {
				t.Fatal("an inexpressible declaration was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not name the declaration: %v", err)
			}
			if strings.Contains(err.Error(), "expected INDENT") {
				t.Errorf("the refusal is the raw parse error: %v", err)
			}
		})
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
