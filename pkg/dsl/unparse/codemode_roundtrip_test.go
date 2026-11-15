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
