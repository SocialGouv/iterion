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
