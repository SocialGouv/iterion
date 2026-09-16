package fix

import (
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

const quotedBot = `dsl: 2

vars:
  base: string = "main"

## The checkout, with the quotes an author writes by reflex.
tool checkout:
  command: "git checkout '{{vars.base}}' && echo \"{{vars.base}}\" && echo 'v={{vars.base}}'"

tool verify:
  command: "test -n {{vars.base}}"
  postcondition: "git rev-parse '{{vars.base}}'"

workflow w:
  worktree: none
  sandbox: none
  entry: checkout
  checkout -> verify
  verify -> done
`

func c137(t *testing.T, src string) []ir.Diagnostic {
	t.Helper()
	pr := parser.Parse("q.bot", src)
	var out []ir.Diagnostic
	for _, d := range ir.Compile(pr.File).Diagnostics {
		if d.Code == ir.DiagQuotedCommandRef {
			out = append(out, d)
		}
	}
	return out
}

// C137's remedy is mechanical when the quotes hug the reference: the
// single-quoted and the escaped double-quoted ones go, in a command and in
// a postcondition, on the original bytes — the comment and the layout stay;
// the quotes that hold more than the reference are left to the author, said
// so; and the fixed text compiles to the same diagnostics minus the fixed.
func TestC137LosesExactlyTheQuotesAroundAReference(t *testing.T) {
	if n := len(c137(t, quotedBot)); n != 4 {
		t.Fatalf("the fixture raises %d C137, want 4", n)
	}
	res, err := Bytes("q.bot", []byte(quotedBot))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || len(res.Applied) != 2 {
		t.Fatalf("applied %+v", res.Applied)
	}
	fixed := string(res.Fixed)
	for _, want := range []string{
		"command: \"git checkout {{vars.base}} && echo {{vars.base}} && echo 'v={{vars.base}}'\"",
		"postcondition: \"git rev-parse {{vars.base}}\"",
		"## The checkout, with the quotes an author writes by reflex.",
		"command: \"test -n {{vars.base}}\"",
	} {
		if !strings.Contains(fixed, want) {
			t.Fatalf("the fixed text lacks %q:\n%s", want, fixed)
		}
	}
	// One C137 remains — the quotes that hold more than the reference, for
	// the author to decide (the message names the reference, not the quotes).
	if left := c137(t, fixed); len(left) != 1 || left[0].NodeID != "checkout" {
		t.Fatalf("after the fix C137 remains for %+v, want the one the author must decide", left)
	}
	var said bool
	for _, l := range res.Left {
		if l.Code == ir.DiagQuotedCommandRef && strings.Contains(l.Why, "more than the reference") {
			said = true
		}
	}
	if !said {
		t.Fatalf("the quotes that hold more than the reference were not said left: %+v", res.Left)
	}
	if res.Applied[0].Line != 8 || res.Applied[1].Line != 12 || !strings.HasPrefix(res.Applied[0].From, `"git checkout '{{vars.base}}'`) {
		t.Fatalf("the edits are not placed on the literals: %+v", res.Applied)
	}
	// Idempotent: the fixed text has nothing mechanical left.
	again, err := Bytes("q.bot", res.Fixed)
	if err != nil || again.Changed {
		t.Fatalf("a second pass changed the text: %v %+v", err, again.Applied)
	}
}

// A file that does not parse is refused; a file with nothing to fix is
// returned as it is, its diagnostics all left with the reason.
func TestFixRefusesAndLeaves(t *testing.T) {
	if _, err := Bytes("b.bot", []byte("agent :\n  model\n")); !errors.Is(err, ErrRefused) {
		t.Fatalf("a broken file: %v", err)
	}
	clean := strings.NewReplacer("'{{vars.base}}'", "{{vars.base}}", `\"{{vars.base}}\"`, "{{vars.base}}", "'v={{vars.base}}'", "{{vars.base}}").Replace(quotedBot)
	res, err := Bytes("c.bot", []byte(clean))
	if err != nil || res.Changed || len(res.Applied) != 0 {
		t.Fatalf("a clean file: %v %+v", err, res)
	}
}

// The proof compares what the fixed text compiles to with what the original
// compiled to minus the fixed — as a multiset: the same text raised twice
// counts twice — and names the first difference.
func TestTheProofCountsDiagnostics(t *testing.T) {
	if why := proven([]string{"a", "b", "b"}, []string{"b", "a", "b"}); why != "" {
		t.Fatalf("the same multiset in another order: %s", why)
	}
	if why := proven([]string{"a", "b"}, []string{"a", "b", "b"}); !strings.Contains(why, "appears that the original had not") {
		t.Fatalf("a diagnostic gained: %q", why)
	}
	if why := proven([]string{"a", "b", "b"}, []string{"a", "b"}); !strings.Contains(why, "gone that no edit fixed") {
		t.Fatalf("a diagnostic lost: %q", why)
	}
}

// PlanFor is the validator's view: the edits for the diagnostics it already
// holds, and the ones it could not place — no recompile.
func TestPlanForAnnotatesDiagnostics(t *testing.T) {
	edits, left := PlanFor("q.bot", []byte(quotedBot), c137(t, quotedBot))
	if len(edits) != 2 || len(left) != 1 || edits[0].Node != "checkout" || edits[1].Node != "verify" {
		t.Fatalf("edits %+v left %+v", edits, left)
	}
	if edits[0].Code != ir.DiagQuotedCommandRef || edits[0].To != "\"git checkout {{vars.base}} && echo {{vars.base}} && echo 'v={{vars.base}}'\"" {
		t.Fatalf("edit %+v", edits[0])
	}
}

// A tool inside a group is instantiated by `use` under a name the source
// has not: its C137 is left to the author, said as a group member — never
// "not found".
func TestAGroupMemberIsSaidAsSuch(t *testing.T) {
	src := `dsl: 2

vars:
  base: string = "main"

group g:
  tool assess:
    command: "git checkout '{{vars.base}}'"

use g as u1

workflow w:
  worktree: none
  sandbox: none
  entry: u1.assess
  u1.assess -> done
`
	edits, left := PlanFor("g.bot", []byte(src), c137(t, src))
	if len(edits) != 0 || len(left) != 1 || left[0].Node != "u1.assess" || !strings.Contains(left[0].Why, "group") {
		t.Fatalf("edits %+v left %+v", edits, left)
	}
}

// A diagnostic is placed only on the literal it names: a command whose
// quotes are not mechanically removable (the same reference twice) beside
// a postcondition whose are — the postcondition's diagnostic lands on the
// postcondition, the command's two are left, and the proof holds.
func TestADiagnosticIsPlacedOnTheLiteralItNames(t *testing.T) {
	src := `dsl: 2

vars:
  x: string = "v"

tool check:
  command: "A='{{vars.x}}' B='{{vars.x}}'"
  postcondition: "test -f '{{vars.x}}'"

workflow w:
  worktree: none
  sandbox: none
  entry: check
  check -> done
`
	res, err := Bytes("p.bot", []byte(src))
	if err != nil {
		t.Fatalf("a fixable file was refused: %v", err)
	}
	if len(res.Applied) != 1 || res.Applied[0].Line != 8 || res.Applied[0].To != "\"test -f {{vars.x}}\"" {
		t.Fatalf("applied %+v", res.Applied)
	}
	if !strings.Contains(string(res.Fixed), "command: \"A='{{vars.x}}' B='{{vars.x}}'\"") {
		t.Fatalf("the command was touched:\n%s", res.Fixed)
	}
	var commandLeft int
	for _, l := range res.Left {
		if l.Code == ir.DiagQuotedCommandRef && strings.Contains(l.Message, "command:") {
			commandLeft++
		}
	}
	if commandLeft != 2 {
		t.Fatalf("the command's two diagnostics were not left: %+v", res.Left)
	}
}

// A raw `{{!ref}}` inside the author's quotes is left alone: the runtime
// does not escape it, so the quotes are its only containment and removing
// them would leave the value bare in the shell — the fixer says so, and
// the plain reference beside it is fixed all the same.
func TestARawReferenceInQuotesIsLeftToTheAuthor(t *testing.T) {
	src := `dsl: 2

vars:
  x: string = "v"

tool run_it:
  command: "A='{{!vars.x}}' B='{{vars.x}}'"

workflow w:
  worktree: none
  sandbox: none
  entry: run_it
  run_it -> done
`
	res, err := Bytes("r.bot", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Fixed), "command: \"A='{{!vars.x}}' B={{vars.x}}\"") {
		t.Fatalf("the raw reference's quotes were touched, or the plain one's kept:\n%s", res.Fixed)
	}
	var said bool
	for _, l := range res.Left {
		if l.Code == ir.DiagQuotedCommandRef && strings.Contains(l.Why, "raw") {
			said = true
		}
	}
	if !said {
		t.Fatalf("the raw reference was not said left: %+v", res.Left)
	}
}
