package parser_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// A `resources:` value may be EITHER a bare integer (counting form) or a
// bracketed ident-list (named-instance lease pool). This covers the text →
// AST direction (Capacities/Members) and the round-trip through unparse.
func TestParseResourcesLeaseAndCountingForms(t *testing.T) {
	src := `tool a:
  command: ` + "`echo hi`" + `
  output: o

schema o:
  ok: bool

workflow t:
  entry: a
  resources:
    godot: ["godot-s1", "godot-s2", "godot-s3"]
    slot: 2
  a -> done
`
	res := parser.Parse("test.bot", src)
	for _, d := range res.Diagnostics {
		t.Fatalf("unexpected diagnostic: %s", d.Error())
	}
	if res.File == nil || len(res.File.Workflows) != 1 {
		t.Fatalf("expected 1 workflow, got file=%v", res.File)
	}
	rb := res.File.Workflows[0].Resources
	if rb == nil {
		t.Fatal("expected a resources block")
	}

	// Lease form → capacity = len(members) + the member ids preserved in order.
	if got := rb.Capacities["godot"]; got != 3 {
		t.Errorf("godot capacity = %d, want 3 (= len of the pool)", got)
	}
	if got := strings.Join(rb.Members["godot"], ","); got != "godot-s1,godot-s2,godot-s3" {
		t.Errorf("godot members = %q, want godot-s1,godot-s2,godot-s3", got)
	}
	// Counting form → capacity only, no members.
	if got := rb.Capacities["slot"]; got != 2 {
		t.Errorf("slot capacity = %d, want 2", got)
	}
	if m, ok := rb.Members["slot"]; ok {
		t.Errorf("slot should have no members (counting form), got %v", m)
	}

	// Round-trip: unparse must emit the lease form as a list and the counting
	// form as an int, and re-parsing must yield the same members.
	out := unparse.Unparse(res.File)
	if !strings.Contains(out, `godot: ["godot-s1", "godot-s2", "godot-s3"]`) {
		t.Errorf("unparse lost the lease list form; output:\n%s", out)
	}
	if !strings.Contains(out, "slot: 2") {
		t.Errorf("unparse lost the counting form; output:\n%s", out)
	}
	res2 := parser.Parse("rt.bot", out)
	for _, d := range res2.Diagnostics {
		t.Fatalf("round-trip diagnostic: %s", d.Error())
	}
	if got := strings.Join(res2.File.Workflows[0].Resources.Members["godot"], ","); got != "godot-s1,godot-s2,godot-s3" {
		t.Errorf("round-trip godot members = %q, want godot-s1,godot-s2,godot-s3", got)
	}
}

// `needs:` on a subbot node must parse like on agent/tool nodes. Regression
// test: `needs` is a keyword token (TokenNeeds), but parseSubbotDecl used to
// match it as TokenIdent — so every subbot `needs:` raised E012 even though
// the AST, the runtime lease wiring, and the docs all support it.
func TestParseSubbotNeeds(t *testing.T) {
	src := `subbot child:
  source: "child.bot"
  with { id: "1" }
  output: o
  needs: slot

schema o:
  ok: bool

workflow t:
  entry: child
  resources:
    slot: 2
  child -> done
`
	res := parser.Parse("test.bot", src)
	for _, d := range res.Diagnostics {
		t.Fatalf("unexpected diagnostic: %s", d.Error())
	}
	if len(res.File.Subbots) != 1 {
		t.Fatalf("expected 1 subbot, got %d", len(res.File.Subbots))
	}
	sd := res.File.Subbots[0]
	if got := strings.Join(sd.Needs, ","); got != "slot" {
		t.Errorf("subbot needs = %q, want slot", got)
	}

	// Bracketed-list form must parse too (parseNeedsList handles both).
	src2 := strings.Replace(src, "needs: slot", "needs: [slot, other]", 1)
	src2 = strings.Replace(src2, "slot: 2", "slot: 2\n    other: 1", 1)
	res2 := parser.Parse("test.bot", src2)
	for _, d := range res2.Diagnostics {
		t.Fatalf("unexpected diagnostic (list form): %s", d.Error())
	}
	if got := strings.Join(res2.File.Subbots[0].Needs, ","); got != "slot,other" {
		t.Errorf("subbot needs (list form) = %q, want slot,other", got)
	}
}
