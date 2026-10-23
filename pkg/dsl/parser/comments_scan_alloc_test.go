package parser

import (
	"strings"
	"testing"
)

// The comment scan Parse runs to hand the comments to the AST reads the
// token stream in place and builds nothing per line of code (Scan's line
// table is the writer's): Parse costs what its lexer costs, give or take the
// program it builds, whether the source's tokens stand on one line or on a
// hundred thousand.
func TestTheCommentScanCostsNothingPerTokenOrLine(t *testing.T) {
	for _, c := range []struct{ name, src string }{
		{"one line of 100 000 names", "agent a:\n  model: \"m\"\n  tools: [" + strings.Repeat("a, ", 100_000) + "a]\n"},
		{"100 000 code lines", strings.Repeat("]\n", 100_000)},
	} {
		t.Run(c.name, func(t *testing.T) {
			lex := allocatedBy(func() { NewLexer("s.bot", c.src) })
			parse := allocatedBy(func() { parseLenient("s.bot", c.src) })
			if extra := int64(parse) - int64(lex); extra > 24<<20 {
				t.Errorf("Parse allocated %d MiB beyond what its lexer did, want under 24 MiB: the comment scan pays per token or per line", extra>>20)
			}
		})
	}
}

// A run of comment lines costs Parse what its AST keeps of them — one
// ast.Comment and one pointer a comment — beyond what its lexer costs: the
// scan hands each comment to the AST the moment it knows its address, so a
// run of comments waits as indices into the token stream, and no comment
// is held twice, once placed and once in the AST.
func TestCommentLinesCostParseTheirASTOnly(t *testing.T) {
	t.Setenv(readBudgetEnv, "")
	const n = 100_000
	for _, c := range []struct{ name, src string }{
		{"at the file's head", strings.Repeat("#\n", n)},
		{"inside a declaration", "agent a:\n  model: \"m\"\n" + strings.Repeat("  # c\n", n)},
	} {
		t.Run(c.name, func(t *testing.T) {
			lex := allocatedBy(func() { NewLexer("c.bot", c.src) })
			var res *ParseResult
			parse := allocatedBy(func() { res = parseLenient("c.bot", c.src) })
			if res.File == nil || len(res.Diagnostics) != 0 {
				t.Fatalf("%d comment lines read with diagnostics: %v", n, first(res.Diagnostics))
			}
			extra := int64(parse) - int64(lex)
			t.Logf("Parse allocated %d MiB beyond its lexer's %d MiB", extra>>20, lex>>20)
			// 100 000 comments: 11 MiB of ast.Comment and a pointer slice
			// grown to hold them, 16 MiB in all; held twice — placed, or
			// collected by the parser and again by the scan — over 30.
			if extra > 24<<20 {
				t.Errorf("Parse allocated %d MiB beyond its lexer for %d comment lines, want under 24 MiB: the comments are held twice", extra>>20, n)
			}
		})
	}
}

// Scan still gives the writer the address of every code line, and a line a
// skipped token stands inside of is read whole.
func TestScanStillAddressesEveryCodeLine(t *testing.T) {
	src := "agent a:\n  model: \"m\"\n  ## note\n  tools: [x]\n"
	lines, comments := Scan(NewLexer("s.bot", src).All())
	if len(lines) != 3 {
		t.Fatalf("Scan addressed %d code lines, want 3: %+v", len(lines), lines)
	}
	if len(comments) != 1 || comments[0].Path != "tools" {
		t.Errorf("the comment is placed at %+v, want before tools", comments)
	}
	// A stream built by hand with a virtual token inside a line: the line
	// is the same either way.
	toks := []Token{
		{Type: TokenAgent, Value: "agent", Line: 1, Column: 1},
		{Type: TokenIndent, Line: 1, Column: 1},
		{Type: TokenIdent, Value: "a", Line: 1, Column: 7},
		{Type: TokenColon, Value: ":", Line: 1, Column: 8},
	}
	cut, _ := Scan(toks)
	whole, _ := Scan([]Token{toks[0], toks[2], toks[3]})
	if len(cut) != 1 || len(whole) != 1 || cut[0].Decl != whole[0].Decl || cut[0].Path != whole[0].Path {
		t.Errorf("a line cut by a virtual token reads as %+v, want %+v", cut, whole)
	}
}
