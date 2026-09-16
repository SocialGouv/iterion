package rewrite

import (
	"strings"
	"testing"
)

// A rewrite lands on the original bytes: a BOM and CRLF line endings are
// kept, and an edit planned on the normalised text replaces exactly the
// bytes it meant to.
func TestARewriteKeepsTheBOMAndTheLineEndings(t *testing.T) {
	src := []byte("\ufeffagent a:\r\n  model: \"m\"\r\n")
	n := Normalize(src)
	if strings.Contains(n.Text, "\r") || strings.HasPrefix(n.Text, "\ufeff") {
		t.Fatalf("the lexer's text still carries a BOM or a CR: %q", n.Text)
	}
	at := strings.Index(n.Text, `"m"`)
	edits := []Edit{{Start: at, End: at + 3, Repl: `"n"`}}
	if _, err := Apply(n.Text, edits); err != nil {
		t.Fatal(err)
	}
	got := string(n.MapBack(src, edits))
	if got != "\ufeffagent a:\r\n  model: \"n\"\r\n" {
		t.Fatalf("mapped back to %q", got)
	}
	if _, err := Apply("0123456789", []Edit{{Start: 0, End: 5, Repl: "a"}, {Start: 3, End: 7, Repl: "b"}}); err == nil {
		t.Fatal("overlapping edits were applied")
	}
}
