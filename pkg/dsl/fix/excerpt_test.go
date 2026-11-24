package fix

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
)

// C137 quotes the reference by excerpt and its readers match the excerpt:
// a reference past the excerpt's length still gets its edit — where a
// reader matching the whole reference found nothing once the diagnostic was
// held to its bound.
func TestAQuotedReferencePastTheExcerptStillGetsItsEdit(t *testing.T) {
	long := "vars." + strings.Repeat("v", 4_100)
	src := "tool t:\n  command: \"echo '{{" + long + "}}'\"\n\nworkflow w:\n  vars:\n    " + long[len("vars."):] + ": string = \"x\"\n  entry: t\n  t -> done\n"
	pr := parsertest.Parse("q.bot", src)
	var diags []ir.Diagnostic
	for _, d := range ir.Compile(pr.File).Diagnostics {
		if d.Code == ir.DiagQuotedCommandRef {
			diags = append(diags, d)
		}
	}
	if len(diags) != 1 {
		t.Fatalf("%d C137, want 1", len(diags))
	}
	edits, left := PlanFor("q.bot", []byte(src), pr.File, diags)
	if len(edits) != 1 || len(left) != 0 {
		t.Fatalf("edits %d, left %d (%+v): the reader did not find the excerpted reference in %q", len(edits), len(left), left, diags[0].Message[:min(len(diags[0].Message), 200)])
	}
}

// The fixer applies to a reference past the excerpt's length wherever it
// stands — at the top level and inside a group used twice — and its own
// bookkeeping (Bytes) recognises each applied edit by the excerpt: a reader
// matching the whole reference refused an ordinary 67-rune one.
func TestTheFixerAppliesToAReferencePastTheExcerpt(t *testing.T) {
	long := "vars." + strings.Repeat("v", 70)
	v := long[len("vars."):]
	top := "tool t:\n  command: \"echo '{{" + long + "}}'\"\n\nworkflow w:\n  vars:\n    " + v + ": string = \"x\"\n  entry: t\n  t -> done\n"
	group := "group g:\n  tool t:\n    command: \"echo '{{" + long + "}}'\"\n\nuse g as i0\nuse g as i1\n\nworkflow w:\n  vars:\n    " + v + ": string = \"x\"\n  entry: i0.t\n  i0.t -> i1.t\n  i1.t -> done\n"
	for name, src := range map[string]string{"top level": top, "group used twice": group} {
		res, err := Bytes("q.bot", []byte(src))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !res.Changed || len(res.Applied) == 0 || len(res.Left) != 0 || strings.Contains(string(res.Fixed), "'{{"+long+"}}'") {
			t.Errorf("%s: changed=%v applied=%d left=%+v — the 70-rune reference kept its quotes", name, res.Changed, len(res.Applied), res.Left)
		}
	}
}
