package unparse_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// `iterion fmt` over both forms of `codemode:`.
//
// The short form has to come back BYTE-IDENTICAL and in its original
// position: every `.bot` that says `codemode: on` predates the bounds, and a
// formatter that reordered those files to say the same thing would put a diff
// in front of reviewers that nobody asked for. The block form has to come
// back as a block, with the implied `on` still implied.

func render(t *testing.T, src string) string {
	t.Helper()
	res, err := parser.Parse("rt.bot", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res.Diagnostics) > 0 {
		t.Fatalf("parse drew diagnostics: %v", res.Diagnostics)
	}
	return unparse.Unparse(res.File)
}

func TestTheShortFormRoundTripsWhereItWasWritten(t *testing.T) {
	t.Parallel()
	src := `agent a:
  model: "m"
  backend: "claw"
  ambient_context: none
  codemode: on
  permission: off

workflow w:
  entry: a
  a -> done
`
	got := render(t, src)
	if !strings.Contains(got, "\n  codemode: on\n") {
		t.Fatalf("the short form did not come back as one line:\n%s", got)
	}
	// Its POSITION, not only its presence: the scalar run it has always
	// lived in. Writing it among the blocks would reorder every existing
	// file that declares it.
	ambient := strings.Index(got, "ambient_context: none")
	cm := strings.Index(got, "codemode: on")
	perm := strings.Index(got, "permission: off")
	if ambient >= cm || cm >= perm {
		t.Errorf("codemode moved out of the ambient_context → permission run:\n%s", got)
	}
	// And the whole file, twice through: a formatter is only idempotent if
	// its own output is a fixed point.
	if again := render(t, got); again != got {
		t.Errorf("not idempotent:\n--- first ---\n%s\n--- second ---\n%s", got, again)
	}
}

func TestTheBlockFormRoundTripsAsABlockWithTheImpliedOnStillImplied(t *testing.T) {
	t.Parallel()
	src := `agent a:
  model: "m"
  backend: "claw"
  codemode:
    tool_calls: 256
    wall: "2m"
    memory: 536870912

workflow w:
  entry: a
  a -> done
`
	got := render(t, src)
	for _, want := range []string{"codemode:", "tool_calls: 256", "wall: \"2m\"", "memory: 536870912"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing from the rendered block:\n%s", want, got)
		}
	}
	// The block implies `on`, so writing it back would add a line the author
	// never wrote — and `iterion fmt` promises the same program, not a
	// longer one that means it.
	if strings.Contains(got, "mode: on") {
		t.Errorf("the implied `on` was written back as a line:\n%s", got)
	}
	if again := render(t, got); again != got {
		t.Errorf("not idempotent:\n--- first ---\n%s\n--- second ---\n%s", got, again)
	}
}

func TestAnExplicitOffInABlockIsWrittenBack(t *testing.T) {
	t.Parallel()
	// The one mode that must survive the round trip: it is what makes the
	// bounds beside it inert (C348), so a formatter that dropped it would
	// turn a reported mistake into a silently different program.
	src := `agent a:
  model: "m"
  backend: "claw"
  codemode:
    mode: off
    tool_calls: 256

workflow w:
  entry: a
  a -> done
`
	got := render(t, src)
	if !strings.Contains(got, "mode: off") {
		t.Fatalf("`mode: off` was dropped, which turns codemode ON:\n%s", got)
	}
	if again := render(t, got); again != got {
		t.Errorf("not idempotent:\n--- first ---\n%s\n--- second ---\n%s", got, again)
	}
}

// The WORKFLOW rung's block, and all fields.
//
// Measured before this test existed: a mutant that stopped writing the
// workflow-level block survived `./pkg/dsl/...` and every importer of this
// package, while `iterion fmt` and the studio save path silently deleted the
// declaration. The block form implies `on`, so the deletion does not merely
// lose the bounds — it turns codemode OFF for every node that inherited
// it. The agent arm above asserted three bounds; a per-field table is
// what makes a dropped line nameable.
func TestTheWorkflowRungsBlockRoundTripsWithEveryBound(t *testing.T) {
	t.Parallel()
	src := `agent a:
  model: "m"
  backend: "claw"

judge j:
  model: "m"
  backend: "claw"
  codemode:
    tool_calls: 99
    result_bytes: 4194304

workflow w:
  entry: a
  codemode:
    tool_calls: 512
    concurrent_tool_calls: 3
    wall: "7m"
    cpu: "90s"
    memory: 1073741824
    script_bytes: 1048576
    result_bytes: 4194304
    tool_result_bytes: 262144
    store_entries: 48
    store_bytes: 262144
  a -> j
  j -> done
`
	got := render(t, src)
	for _, want := range []string{
		"tool_calls: 512", "concurrent_tool_calls: 3", "wall: \"7m\"", "cpu: \"90s\"", "memory: 1073741824",
		"script_bytes: 1048576", "result_bytes: 4194304", "tool_result_bytes: 262144",
		"store_entries: 48", "store_bytes: 262144",
		// and the judge's own block, the host the agent arm does not cover
		"tool_calls: 99",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing from the rendered document:\n%s", want, got)
		}
	}
	if again := render(t, got); again != got {
		t.Errorf("not idempotent:\n--- first ---\n%s\n--- second ---\n%s", got, again)
	}
}

// A declared `defer` takes the block form — the short form has no way to
// carry it, and collapsing it there would silently turn the deferral off,
// the program's meaning changing under a formatter that promised to keep it.
func TestADeclaredDeferRoundTripsAsABlock(t *testing.T) {
	t.Parallel()
	src := `agent a:
  model: "m"
  backend: "claw"
  codemode:
    defer: on
    tool_calls: 128

workflow w:
  entry: a
  a -> done
`
	got := render(t, src)
	if !strings.Contains(got, "defer: on") {
		t.Fatalf("the declared defer was dropped, which turns the deferral off:\n%s", got)
	}
	if strings.Contains(got, "\n  codemode: on\n") {
		t.Errorf("a defer-only neighbourhood collapsed to the short form, which cannot carry it:\n%s", got)
	}
	// Written as declared: `defer: off` is not an implied value to omit.
	src = `agent a:
  model: "m"
  backend: "claw"
  codemode:
    mode: off
    defer: off

workflow w:
  entry: a
  a -> done
`
	got = render(t, src)
	if !strings.Contains(got, "defer: off") {
		t.Fatalf("the declared `defer: off` was dropped:\n%s", got)
	}
	if again := render(t, got); again != got {
		t.Errorf("not idempotent:\n--- first ---\n%s\n--- second ---\n%s", got, again)
	}
}
