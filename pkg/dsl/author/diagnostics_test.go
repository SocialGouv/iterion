package author

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A prompt body the lexer settles otherwise than written reads without an
// error, and the reading is SAID: a warning names what is dropped, at the
// body's line; the document still has no error.
func TestAPromptBodyTheLexerSettlesIsWarned(t *testing.T) {
	path := filepath.Join(fixtureDir("valid"), "prompt-body-settled.yaml")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	res := Parse("settled.yaml", src)
	if res.HasErrors() {
		t.Fatalf("the document is refused: %v", res.Diagnostics)
	}
	var warned *parser.Diagnostic
	for i, d := range res.Diagnostics {
		if d.Code == parser.DiagAuthorPromptBody && d.Severity == parser.SeverityWarning {
			warned = &res.Diagnostics[i]
		}
	}
	if warned == nil {
		t.Fatalf("no E053 warning among %v", res.Diagnostics)
	}
	if want := lineOf(string(src), "  spaced: |"); warned.Line != want {
		t.Errorf("the warning is at line %d, want %d (the body's)", warned.Line, want)
	}
	if !strings.Contains(warned.Message, "leading blank line") {
		t.Errorf("the warning does not name the dropped lines: %s", warned.Message)
	}
	if body := res.File.Prompts[0].Body; body != "Indented and surrounded by blank lines.\nSecond line." {
		t.Errorf("the body read is %q, not the settled form", body)
	}
}

// A YAML syntax error arrives with the spelling that avoids it, not only
// the scanner's own words: a plain value holding `: ` — the trap a shell
// command with a jq filter falls into — says "quote the whole value".
func TestAYAMLSyntaxErrorNamesItsRemedy(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(fixtureDir("invalid"), "plain-value-holds-a-colon.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	res := Parse("colon.yaml", src)
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].Code != parser.DiagAuthorDocument {
		t.Fatalf("want one E050, got %v", res.Diagnostics)
	}
	d := res.Diagnostics[0]
	if d.Line != lineOf(string(src), "    command: go test") {
		t.Errorf("positioned at line %d, want the command's line", d.Line)
	}
	if !strings.Contains(d.Message, "quote the whole value") {
		t.Errorf("the message does not name the remedy: %s", d.Message)
	}
}

// HasErrors is what a caller acts on: true on an error, false on warnings
// alone and on a clean document.
func TestHasErrorsTellsErrorsFromWarnings(t *testing.T) {
	clean, err := os.ReadFile(filepath.Join(fixtureDir("valid"), "minimal-v1.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if res := Parse("clean.yaml", clean); res.HasErrors() {
		t.Errorf("a clean document has errors: %v", res.Diagnostics)
	}
	warned, err := os.ReadFile(filepath.Join(fixtureDir("valid"), "prompt-body-settled.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if res := Parse("warned.yaml", warned); res.HasErrors() || len(res.Diagnostics) == 0 {
		t.Errorf("a warned document has errors, or no diagnostic: %v", res.Diagnostics)
	}
	if res := Parse("bad.yaml", []byte("dsl: 2\nnodes:\n  - agent: a\n    max_tokens: -1\n")); !res.HasErrors() {
		t.Errorf("a refused document has no error: %v", res.Diagnostics)
	}
}
