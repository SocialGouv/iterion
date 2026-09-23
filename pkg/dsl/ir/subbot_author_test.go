package ir

import (
	"strings"
	"testing"
)

// A subbot's child is a .bot: a `source:` naming an author document is
// refused where the parent is compiled — validate and launch alike — since
// the runtime resolver runs after the parent has launched, and a snapshot
// bypasses it.
func TestSubbotSourceMustBeABotNotAnAuthorDocument(t *testing.T) {
	src := `
schema empty:
  ok: bool

subbot child:
  source: "child.bot.yaml"
  output: empty

tool a:
  command: "true"
  output: empty

workflow w:
  entry: a
  a -> child
  child -> done
`
	if !hasDiag(compileFile(t, src).Diagnostics, DiagSubbotAuthorSource) {
		t.Fatal("expected C305 (DiagSubbotAuthorSource) for a child named as an author document")
	}
	bot := strings.Replace(src, "child.bot.yaml", "child.bot", 1)
	if hasDiag(compileFile(t, bot).Diagnostics, DiagSubbotAuthorSource) {
		t.Fatal("a .bot child is refused as an author document")
	}
}
