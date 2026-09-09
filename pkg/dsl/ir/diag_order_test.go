package ir

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// The compiler's own list puts a finding without a position AFTER the
// positioned ones: `entry node "ghost" not found` (global) is read after the
// edge that names an unknown node, not before it.
func TestGlobalFindingsSortAfterPositionedOnes(t *testing.T) {
	src := "schema out:\n  ok: bool\n\nagent a:\n  model: \"m\"\n  output: out\n\nworkflow w:\n  entry: ghost\n  a -> zzz\n"
	pr := parser.Parse("order.bot", src)
	for _, d := range pr.Diagnostics {
		t.Fatalf("unexpected parse diagnostic: %s", d.Error())
	}
	res := Compile(pr.File)
	seenGlobal := false
	var positioned, global int
	for _, d := range res.Diagnostics {
		if d.Line == 0 {
			global++
			seenGlobal = true
			continue
		}
		positioned++
		if seenGlobal {
			t.Fatalf("positioned finding %s [%s] listed after a global one:\n%v", d.Message, d.Code, res.Diagnostics)
		}
	}
	if positioned == 0 || global == 0 {
		t.Fatalf("fixture must yield both kinds, got %d positioned / %d global:\n%v", positioned, global, res.Diagnostics)
	}
}
