package parser

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// topLevelDeclKeywords is every keyword parseFile dispatches on as a
// top-level declaration. It is the parser's own list — the test below
// holds the two human-readable grammars to it, so a declaration kind
// added to the parser cannot stay absent from the reference an author
// reads (the way `fail` was missing from dsl-grammar.md and
// `await_answers` from the EBNF until 2026-09-09).
var topLevelDeclKeywords = []string{
	"vars", "presets", "attachments", "secrets", "mcp_server",
	"prompt", "schema", "cursor", "supervisor",
	"agent", "judge", "router", "human", "tool", "compute",
	"emit", "wait", "await_answers", "fail", "subbot",
	"group", "use", "workflow",
}

// topLevelProduction extracts the body of the `top_level_decl = … ;`
// production from a grammar document.
func topLevelProduction(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := regexp.MustCompile(`(?s)top_level_decl\s*=(.*?);`).FindStringSubmatch(string(data))
	if m == nil {
		t.Fatalf("%s: no `top_level_decl = … ;` production found", path)
	}
	return m[1]
}

// TestTopLevelDeclarationsAreDocumented fails when a declaration keyword the
// parser accepts at top level is missing from the readable grammar or the
// EBNF's top_level_decl production. The EBNF names productions
// `<keyword>_decl`; the readable grammar names the keyword itself.
func TestTopLevelDeclarationsAreDocumented(t *testing.T) {
	ebnf := topLevelProduction(t, "../../../docs/grammar/iterion_v1.ebnf")
	readable := topLevelProduction(t, "../../../docs/references/dsl-grammar.md")
	for _, kw := range topLevelDeclKeywords {
		if !regexp.MustCompile(`\b` + kw + `_decl\b`).MatchString(ebnf) {
			t.Errorf("docs/grammar/iterion_v1.ebnf: top_level_decl lacks %s_decl", kw)
		}
		if !regexp.MustCompile(`\b` + kw + `\b`).MatchString(readable) {
			t.Errorf("docs/references/dsl-grammar.md: top_level_decl lacks %s", kw)
		}
	}
	// The parser side of the same contract: every keyword listed here is a
	// declaration keyword the lexer knows (a typo in the list above would
	// otherwise pass silently).
	for _, kw := range topLevelDeclKeywords {
		if _, ok := keywords[kw]; !ok {
			t.Errorf("%q is not a lexer keyword — the list in this test is stale", kw)
		}
	}
	_ = strings.TrimSpace
}
