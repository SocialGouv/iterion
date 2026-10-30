package parser

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
)

// #2779: an identifier was one token of the read budget WHATEVER its length,
// and every per-reference diagnostic carries its container's name — the
// prompt, the node id (node_id, which the studio reads to locate the node
// and so cannot excerpt), the mapping key — whole. A name of megabytes rode
// every message that named it. The bound is lexical, like the size cap: an
// identifier past MaxIdentifierRunes runes is refused as it is scanned
// (E057), so no message downstream can carry an unbounded name — and the
// refusal itself holds no copy of it.
func TestAnIdentifierPastTheCapIsRefusedBounded(t *testing.T) {
	long := strings.Repeat("n", MaxIdentifierRunes+1000)
	res, err := Parse("x.bot", "agent "+long+":\n  model: \"m\"\n\nworkflow w:\n  entry: done\n")
	if err != nil {
		t.Fatalf("a long identifier is a diagnostic, not a whole-source refusal: %v", err)
	}
	var d *Diagnostic
	for i := range res.Diagnostics {
		if res.Diagnostics[i].Code == DiagIdentifierTooLong {
			d = &res.Diagnostics[i]
			break
		}
	}
	if d == nil {
		t.Fatalf("no E057 in %+v", res.Diagnostics)
	}
	if d.Line != 1 {
		t.Errorf("E057 at line %d, want 1 — the identifier's own line", d.Line)
	}
	if len(d.Message) > expr.MaxDiagnosticText+len("…") || len(d.Hint) > expr.MaxDiagnosticText+len("…") {
		t.Errorf("the refusal carries the name: %d / %d bytes", len(d.Message), len(d.Hint))
	}
	if strings.Contains(d.Message, long) {
		t.Error("the refusal quotes the name whole")
	}
	if d.Hint == "" {
		t.Error("E057 arrives without its remedy")
	}
}

// A name at the cap reads; one past it does not.
func TestAnIdentifierAtTheCapReads(t *testing.T) {
	name := strings.Repeat("n", MaxIdentifierRunes)
	res, err := Parse("x.bot", "agent "+name+":\n  model: \"m\"\n\nworkflow w:\n  entry: "+name+"\n  "+name+" -> done\n")
	if err != nil {
		t.Fatalf("a name at the cap: %v", err)
	}
	for _, d := range res.Diagnostics {
		if d.Code == DiagIdentifierTooLong {
			t.Fatalf("E057 on a name of exactly %d runes", MaxIdentifierRunes)
		}
	}
	if res.File == nil || len(res.File.Agents) != 1 {
		t.Fatalf("the file did not parse: %+v", res.Diagnostics)
	}
}

// The token stream itself holds no copy of the refused name: the diagnostic
// token's value is the bounded message.
func TestTheStreamHoldsNoCopyOfARefusedIdentifier(t *testing.T) {
	long := strings.Repeat("n", MaxIdentifierRunes*4)
	l := NewLexer("x.bot", "agent "+long+":\n  model: \"m\"\n")
	for _, tok := range l.All() {
		if len(tok.Value) > MaxIdentifierRunes {
			t.Fatalf("a token carries %d runes: %.80s…", len(tok.Value), tok.Value)
		}
	}
}
